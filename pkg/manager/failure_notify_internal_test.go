package manager

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A failed download must reach the arr now: it only fails a grab it sees in
// history, and a busy arr may not refresh before the entry is swept.
func TestFailedJobAsksArrToRefreshDownloads(t *testing.T) {
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
	m := withTestConfig(t, &Manager{queue: newQueue(store, "", nil, zerolog.Nop()), logger: zerolog.Nop()})
	m.arr.AddOrUpdate(arr.Arr{Name: "radarr", Host: server.URL, Token: "token", Type: arr.Radarr})
	m.jobQueue = NewJobQueue(t.Context(), 1, m.processJob, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)

	entry := &storage.Entry{InfoHash: "abc", Name: "Movie", Category: "radarr", Protocol: config.ProtocolNZB}
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	m.handleJobError(&Job{ID: "abc", Type: JobTypeNZB, Entry: entry}, errors.New("usenet parse failed"))

	select {
	case body := <-refreshed:
		if !strings.Contains(body, "RefreshMonitoredDownloads") {
			t.Fatalf("arr command = %s, want RefreshMonitoredDownloads", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("arr was not asked to refresh its downloads after the failure")
	}
}
