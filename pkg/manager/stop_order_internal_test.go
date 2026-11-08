package manager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/rs/zerolog"
)

// messages lists every logged message, in order.
func (l *lockedLog) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var messages []string
	scanner := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
	for scanner.Scan() {
		var line struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err == nil {
			messages = append(messages, line.Message)
		}
	}
	return messages
}

// The services registerServices makes stop in the reverse of the order it makes them in, and storage closes after
// the last of them.
func TestRegisteredServicesStopInReverseOrderAndStorageLast(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	logs := &lockedLog{}
	m.logger = zerolog.New(logs)

	stopRoot(t, m.group)

	want := []string{
		"stopping", "scheduler", "repair", "indexer", "reacquire", "placements", "usenet", "hearsay",
	}
	if got := logs.stopped(); !slices.Equal(got, want) {
		t.Fatalf("stop order = %v, want %v", got, want)
	}
	messages := logs.messages()
	storage, lastService := slices.Index(messages, "Closing storage database"), -1
	for i, message := range messages {
		if message == "Service stopped" {
			lastService = i
		}
	}
	if storage < 0 || storage < lastService {
		t.Fatalf("storage closed at %d, before the last service stopped at %d: %v", storage, lastService, messages)
	}
}

// Notification deliveries stop after every service, so a service may still notify while it stops.
func TestNotificationsStopAfterEveryService(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	logs := &lockedLog{}
	m.logger = zerolog.New(logs)
	seen := make(chan []string, 1)
	if err := m.notifyGroup.Go("probe", func(ctx context.Context) error {
		<-ctx.Done() // the notifications group begins to stop
		seen <- logs.messages()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	stopRoot(t, m.group)

	stopped := 0
	for _, message := range <-seen {
		if message == "Service stopped" {
			stopped++
		}
	}
	if want := len(logs.stopped()); stopped != want || want == 0 {
		t.Fatalf("notifications began to stop after %d of %d services", stopped, want)
	}
}
