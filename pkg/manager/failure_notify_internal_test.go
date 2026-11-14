package manager

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
)

// A failed download must reach the arr now: it only fails a grab it sees in
// history, and a busy arr may not refresh before the entry is swept.
func TestFailedGrabAsksArrToRefreshDownloads(t *testing.T) {
	t.Parallel()
	refreshed := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && r.URL.Path == "/api/v3/command" {
			refreshed <- string(body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":1,"name":"RefreshMonitoredDownloads","status":"queued"}`)
	}))
	t.Cleanup(server.Close)

	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{queue: newQueue(store, nil, &grabIndex{}, zerolog.Nop()), logger: zerolog.Nop()})
	m.arr.AddOrUpdate(arr.Arr{Name: "radarr", Host: server.URL, Token: "token", Type: arr.Radarr})

	entry := &storage.Entry{InfoHash: "abc", Name: "Movie", Category: "radarr", Protocol: config.ProtocolNZB}
	m.failEffects(failedGrab(entry, "usenet parse failed"), grab.FailEntry)

	select {
	case body := <-refreshed:
		if !strings.Contains(body, "RefreshMonitoredDownloads") {
			t.Fatalf("arr command = %s, want RefreshMonitoredDownloads", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("arr was not asked to refresh its downloads after the failure")
	}
}

// Every failure path must release a parsed NZB's usenet side.
func TestFailEffectsReleaseParsedNZB(t *testing.T) {
	t.Parallel()
	usenetClient := newUnreachableUsenet(t)
	parsing := &storage.NZB{ID: "nzb-1", Status: usenet.NZBStatusParsing}
	if addErr := usenetClient.NZBStorage().AddNZB(parsing); addErr != nil {
		t.Fatal(addErr)
	}
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	queue := newQueue(store, nil, &grabIndex{}, zerolog.Nop())
	m := withTestConfig(t, &Manager{usenet: usenetClient, queue: queue, logger: zerolog.Nop()})
	entry := &storage.Entry{InfoHash: "nzb-1", Name: "release", Protocol: config.ProtocolNZB}

	m.failEffects(failedGrab(entry, "nzb rejected: hearsay claims segments missing"), grab.FailEntry)

	header, getErr := usenetClient.GetNZBHeader("nzb-1")
	if getErr != nil || header.Status != usenet.NZBStatusFailed {
		t.Fatalf("usenet record = %+v, %v; want failed", header, getErr)
	}
}

// Nothing re-reads a failed grab's staged NZB, so it goes at the failure instead of lingering through the 24h hold.
func TestFailEffectsDropStagedNZB(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	staged := filepath.Join(t.TempDir(), "job.queued")
	if writeErr := os.WriteFile(staged, []byte("<nzb/>"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	queue := newQueue(store, nil, &grabIndex{}, zerolog.Nop())
	m := withTestConfig(t, &Manager{queue: queue, logger: zerolog.Nop()})
	entry := &storage.Entry{InfoHash: "job", Name: "release", Protocol: config.ProtocolNZB, Magnet: staged}

	m.failEffects(failedGrab(entry, "usenet parse failed"), grab.FailEntry)

	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatalf("staged NZB still on disk after the failure: %v", statErr)
	}
}

func failedGrab(entry *storage.Entry, reason string) grab.Grab {
	return grab.Grab{
		State: grab.State{Key: entry.InfoHash, Phase: grab.PhaseFailed, Failure: grab.Failure{Reason: reason}},
		Entry: entry,
	}
}
