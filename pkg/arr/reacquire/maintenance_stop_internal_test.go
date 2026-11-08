package reacquire

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// owedRemonitorRig is a rig whose job waits with a re-monitor due, and whose Sonarr holds the next monitor request
// until that request's context ends. entered receives once the request has arrived.
func owedRemonitorRig(t *testing.T) (*fakeSonarrRig, <-chan struct{}) {
	t.Helper()
	fake := newFakeSonarr(t, 101)
	fake.setMonitorFailing(true)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	entered := make(chan struct{}, 1)
	fake.onMonitor = func(request *http.Request) {
		// The server notices a client that went away only once the body is read.
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read monitor request: %v", err)
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		select {
		case entered <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}
	rig.advance(remonitorBackoff + time.Second)
	return rig, entered
}

// A maintenance pass runs on the worker's context, so a stop does not wait for the Arr call it is in.
func TestMaintenancePassStopsWithTheWorker(t *testing.T) {
	t.Parallel()
	rig, entered := owedRemonitorRig(t)
	service := rig.service
	if err := service.worker.Go("maintenance", func(ctx context.Context) error {
		service.maintainJobs(ctx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-entered

	service.g.Close()
	select {
	case <-service.worker.Done():
	case <-time.After(deadlockWatchdog):
		t.Fatal("the worker did not stop: its maintenance pass kept running on a context that ends after it")
	}
}

// A binding indexed from another goroutine completes its job on a context that ends when the worker stops, not
// after: the caller does not hold the service's operation guard past the stop.
func TestIndexedBindingCompletionStopsWithTheWorker(t *testing.T) {
	t.Parallel()
	rig, entered := owedRemonitorRig(t)
	service := rig.service
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	if err := service.worker.Go("hold", func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	replacement := fakeSonarrBinding(rig.fake.instance(), 101)
	replacement.EntryID, replacement.EntryFileID, replacement.DownloadID = "entry-2", "file-2", "download-2"
	replacement.ArrFileID, replacement.LibraryPath = fakeSonarrFileID+1, "/library/show/s01e01-new.mkv"
	upserted := make(chan struct{})
	go func() {
		defer close(upserted)
		_ = service.UpsertBinding(replacement)
	}()
	<-entered

	service.g.Close()
	select {
	case <-upserted:
	case <-time.After(deadlockWatchdog):
		t.Fatal("UpsertBinding kept running after the worker was told to stop")
	}
	select {
	case <-service.worker.Done():
		t.Fatal("the worker finished while its hold task was still running")
	default:
	}
	if err := service.g.Context().Err(); err != nil {
		t.Fatalf("the group's context ended before its worker stopped: %v", err)
	}
}
