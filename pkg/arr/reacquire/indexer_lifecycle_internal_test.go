package reacquire

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/state/statetest"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

func TestIndexerLifecycleTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, indexerSuccessors,
		[]indexerState{indexerConstructed, indexerRunning, indexerStopped},
		[]statetest.Edge[indexerState]{
			{From: indexerConstructed, To: indexerRunning},
			{From: indexerConstructed, To: indexerStopped},
			{From: indexerRunning, To: indexerStopped},
		})
}

// heldCatalog blocks every read, ignoring its context, until release is closed. entered receives once a read began.
type heldCatalog struct {
	entered chan struct{}
	release chan struct{}
}

func (c heldCatalog) ListManagedFiles(context.Context, string) ([]ManagedFile, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.release
	return nil, nil
}

func newStartableIndexer(t *testing.T, g *task.Group, catalog ManagedCatalog) *Indexer {
	t.Helper()
	arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	return NewIndexer(g, arrs, catalog, new(recordingBindingWriter), t.TempDir(), zerolog.Nop())
}

// Guard of the new design: once the group has stopped, nothing is queued, whether or not anybody called Close.
// The indexer's loop is held in its first read, so the group's context ends while the state is still running.
func TestIndexerReindexAfterGroupStopReturnsFalse(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	catalog := heldCatalog{entered: make(chan struct{}, 1), release: make(chan struct{})}
	indexer := newStartableIndexer(t, g, catalog)
	if err := indexer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !indexer.ReindexEntry("radarr", "entry") {
		t.Fatal("ReindexEntry while running = false")
	}
	<-catalog.entered

	g.Close()
	<-g.Context().Done()
	if indexer.ReindexEntry("radarr", "entry-2") {
		t.Fatal("ReindexEntry after the group's context ended, before its loop left = true")
	}

	close(catalog.release)
	<-g.Done()
	if indexer.ReindexEntry("radarr", "entry-3") {
		t.Fatal("ReindexEntry after the group stopped = true")
	}
	if indexer.Refresh() {
		t.Fatal("Refresh after the group stopped = true")
	}
}

// Guard of the new design: delayed retries scheduled while the group stops either run as tasks the group waits for
// or are refused; none starts after the group is done.
func TestIndexerRetryRacingClose(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	indexer := newStartableIndexer(t, g, fixedManagedCatalog(nil))
	if err := indexer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	var scheduled sync.WaitGroup
	for n := range 8 {
		scheduled.Go(func() {
			request := indexRequest{arrName: "radarr", entryID: string(rune('a' + n))}
			for range 20 {
				indexer.retry(request, time.Duration(n)*time.Millisecond)
			}
		})
	}
	indexer.Close()
	scheduled.Wait()

	if left := g.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after Close: %v", left)
	}
	indexer.retry(indexRequest{arrName: "radarr", entryID: "late"}, time.Nanosecond)
	if left := g.Snapshot(); len(left) != 0 {
		t.Fatalf("a retry started after the group was done: %v", left)
	}
}

func TestIndexerSnapshotEmptyAfterStop(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	indexer := newStartableIndexer(t, g, fixedManagedCatalog(nil))
	if err := indexer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	indexer.retry(indexRequest{arrName: "radarr", entryID: "entry"}, time.Hour)
	if len(g.Snapshot()) < 2 {
		t.Fatalf("snapshot while running = %v, want the loop and the pending retry", g.Snapshot())
	}

	stopGroup(g)

	if left := g.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after the stop: %v", left)
	}
}

func TestIndexerStopWithoutStart(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	indexer := newStartableIndexer(t, g, fixedManagedCatalog(nil))

	stopGroup(g)

	if indexer.Refresh() {
		t.Fatal("Refresh on an indexer that never started = true")
	}
	if err := indexer.Start(t.Context()); !errors.Is(err, errIndexerClosed) {
		t.Fatalf("Start after the group stopped = %v, want %v", err, errIndexerClosed)
	}
	indexer.lifecycleMu.Lock()
	got := indexer.state
	indexer.lifecycleMu.Unlock()
	if got != indexerStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
}
