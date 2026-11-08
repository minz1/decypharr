package manager

import (
	"bufio"
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
)

// lockedLog is a log sink many tasks can write to. onStopped, when set, is told the name of each service the moment
// it logs "Service stopped", so a test can put service stops in the same ordered list as its own events.
type lockedLog struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	onStopped func(service string)
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	n, err := l.buf.Write(p)
	l.mu.Unlock()
	if l.onStopped != nil {
		var line struct {
			Service string `json:"service"`
			Message string `json:"message"`
		}
		if json.Unmarshal(p, &line) == nil && line.Message == "Service stopped" {
			l.onStopped(line.Service)
		}
	}
	return n, err
}

// stopped lists the services that logged "Service stopped", in order.
func (l *lockedLog) stopped() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var names []string
	scanner := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
	for scanner.Scan() {
		var line struct {
			Service string `json:"service"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err == nil && line.Message == "Service stopped" {
			names = append(names, line.Service)
		}
	}
	return names
}

func (l *lockedLog) contains(text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), text)
}

// slowDeleteProvider takes delay to delete a torrent and ignores every context, as the provider clients do.
type slowDeleteProvider struct {
	debrid.Client

	delay   time.Duration
	deleted func()
}

func (p slowDeleteProvider) DeleteTorrent(string) error {
	time.Sleep(p.delay)
	p.deleted()
	return nil
}

// checkPlacementStopOrder checks the events of a stop with a slow delete in flight. Everything that can queue a
// delete stopped before the placements group, and none of it waited for the delete. The group's stop waits for the
// delete, so usenet and then hearsay stop only after it.
func checkPlacementStopOrder(t *testing.T, events []string) {
	t.Helper()
	at := func(event string) int { return slices.Index(events, event) }
	finished := at("delete finished")
	if finished < 0 {
		t.Fatalf("events = %v, the delete never finished", events)
	}
	for _, service := range []string{"repair", "indexer", "reacquire"} {
		stopped := at("stopped " + service)
		if stopped < 0 || stopped > at("stopped placements") || stopped > finished {
			t.Errorf("%s did not stop before the delete finished and the placements group: %v", service, events)
		}
	}
	usenet := at("stopped usenet")
	if at("stopped placements") >= usenet || finished >= usenet || usenet >= at("stopped hearsay") {
		t.Errorf("events = %v, want the delete finished before usenet stops, and hearsay last", events)
	}
}

// A provider delete in flight when the manager stops delays only the services that stop after the placements
// group (usenet, hearsay) and storage: the repair and Arr stops do not wait for it, and the whole stop fits
// the budget. A delete asked for once that group has closed is refused and logged.
func TestSlowProviderDeleteKeepsTheStopOrderAndFitsTheBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const (
			deleteTime = 20 * time.Second
			budget     = 25 * time.Second
		)
		m := newShutdownTestManager(t, t.TempDir())
		var mu sync.Mutex
		var events []string
		record := func(event string) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, event)
		}
		logs := &lockedLog{onStopped: func(service string) { record("stopped " + service) }}
		m.logger = zerolog.New(logs)
		client := slowDeleteProvider{delay: deleteTime, deleted: func() { record("delete finished") }}
		m.startPlacementRun("Show.S01", "torbox", func() { _ = client.DeleteTorrent("id") })
		synctest.Wait()

		started := time.Now()
		ctx, cancel := task.Detached(t.Context(), budget)
		defer cancel()
		if err := m.group.Stop(ctx); err != nil {
			t.Fatalf("Stop = %v, want it to fit the %v budget", err, budget)
		}
		if elapsed := time.Since(started); elapsed != deleteTime {
			t.Fatalf("Stop took %v, want the %v the delete needed", elapsed, deleteTime)
		}
		mu.Lock()
		got := slices.Clone(events)
		mu.Unlock()
		checkPlacementStopOrder(t, got)

		if m.startPlacementRun("Show.S02", "realdebrid", func() { _ = client.DeleteTorrent("late") }) {
			t.Fatal("a delete after the placements group closed was accepted")
		}
		synctest.Wait()
		for _, want := range []string{"Show.S02", "realdebrid", "may be left behind"} {
			if !logs.contains(want) {
				t.Errorf("the refused delete was not logged with %q", want)
			}
		}
	})
}
