package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// reacquireHandler is the manual reacquire endpoint over a real manager whose Arr never answers, so a job it
// creates stays active for the whole test.
func reacquireHandler(t *testing.T) (*Server, func(body string) (int, string), func(n int)) {
	t.Helper()
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() {
		close(release)
		stalled.Close()
	})
	mgr, _ := managertest.New(t, func(cfg *config.Config) {
		cfg.Arrs = []config.Arr{{Name: "radarr", Host: stalled.URL, Token: "token"}}
	})
	instance, ok := mgr.Arr().Get("radarr")
	if !ok {
		t.Fatal("radarr is not configured")
	}
	service := mgr.ArrService()
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	// bind adds a movie n with its own entry, file and download; entry-1 is movie 0.
	bind := func(n int) {
		binding := reacquire.Binding{
			ArrName: "radarr", ArrType: arr.Radarr, ArrInstanceFingerprint: instance.Fingerprint(),
			EntryID: fmt.Sprintf("entry-%d", n+1), EntryFileID: "file", DownloadID: fmt.Sprintf("download-%d", n),
			ArrFileID: 40 + n, LibraryPath: fmt.Sprintf("/library/movie-%d.mkv", n), MovieID: 7 + n,
			Confidence: reacquire.ConfidenceExactPath,
		}
		if err := service.UpsertBinding(binding); err != nil {
			t.Fatal(err)
		}
	}
	bind(0)
	s := &Server{manager: mgr, logger: zerolog.Nop()}
	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.handleArrReacquire(w, httptest.NewRequest(http.MethodPost, "/api/arr/reacquire", strings.NewReader(body)))
		var job struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &job)
		return w.Code, job.ID
	}
	return s, post, bind
}

// A manual reacquisition deletes an Arr file, so a tripped breaker holds it unless forced, but asking again for a
// file that already has a job returns that job whatever the breaker says: it deletes nothing.
func TestManualReacquireHandlerPassesTheBreakerOnlyForNewJobs(t *testing.T) {
	t.Parallel()
	s, post, _ := reacquireHandler(t)
	tripUnclassified(t, s.manager.Repair())
	const body = `{"entryId":"entry-1","fileId":"file","cause":"manual","strategy":"command_search"}`

	if code, id := post(body); code != http.StatusConflict || id != "" {
		t.Fatalf("a new reacquisition in a tripped bucket = %d (job %q), want the breaker's conflict", code, id)
	}
	code, first := post(strings.Replace(body, `}`, `,"force":true}`, 1))
	if code != http.StatusAccepted || first == "" {
		t.Fatalf("forced reacquisition = %d (job %q), want accepted with a job", code, first)
	}
	code, second := post(body)
	if code != http.StatusAccepted || second != first {
		t.Fatalf("a repeat without force = %d (job %q), want the existing job %q returned", code, second, first)
	}
}
