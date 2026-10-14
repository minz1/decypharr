package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/sessions"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

type fakePruner struct {
	last   manager.PruneReport
	report manager.PruneReport
	err    error
	opts   manager.PruneOptions
}

func (f *fakePruner) LastPruneReport() manager.PruneReport { return f.last }

func (f *fakePruner) RunPruneNow(opts manager.PruneOptions) (manager.PruneReport, error) {
	f.opts = opts
	return f.report, f.err
}

func pruneRequest(t *testing.T, handler http.HandlerFunc, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(method, target, nil))
	return response
}

func decodeReport(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", response.Body.String(), err)
	}
	return body
}

func TestPruneStatusReturnsLastReport(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	s := &Server{prune: &fakePruner{last: manager.PruneReport{
		StartedAt: started, Referenced: 7, Candidates: []manager.PruneCandidate{{InfoHash: "abc", Excluded: "manual"}},
	}}}

	response := pruneRequest(t, s.handlePruneStatus, http.MethodGet, "/api/prune/status")

	body := decodeReport(t, response)
	if response.Code != http.StatusOK || body["started_at"] != "2026-10-13T12:00:00Z" ||
		body["referenced"] != float64(7) {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	candidate := body["candidates"].([]any)[0].(map[string]any)
	if candidate["info_hash"] != "abc" || candidate["excluded"] != "manual" {
		t.Fatalf("candidate %v", candidate)
	}
	if _, ok := candidate["eligible_at"]; ok {
		t.Fatalf("zero time not omitted: %v", candidate)
	}
}

func TestPruneRunMapsResults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		rep  manager.PruneReport
		want int
	}{
		{"disabled", manager.ErrPruneDisabled, manager.PruneReport{}, http.StatusConflict},
		{"ok", nil, manager.PruneReport{Deleted: 2}, http.StatusOK},
		{"aborted", errors.New("boom"), manager.PruneReport{Aborted: "boom"}, http.StatusOK},
		{"cancelled", context.Canceled, manager.PruneReport{}, http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{prune: &fakePruner{report: test.rep, err: test.err}}
			response := pruneRequest(t, s.handlePruneRun, http.MethodPost, "/api/prune/run")
			if response.Code != test.want {
				t.Fatalf("code=%d want %d body=%s", response.Code, test.want, response.Body.String())
			}
			if test.name == "aborted" && decodeReport(t, response)["aborted"] != "boom" {
				t.Fatalf("body %s", response.Body.String())
			}
			if test.want != http.StatusOK && decodeReport(t, response)["error"] == nil {
				t.Fatalf("no error body: %s", response.Body.String())
			}
		})
	}
}

func TestPruneRunIgnoresRequestContext(t *testing.T) {
	t.Parallel()
	s := &Server{prune: &fakePruner{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/prune/run", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	s.handlePruneRun(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("code=%d", response.Code)
	}
}

func TestPruneRunPassesAcceptBaseline(t *testing.T) {
	t.Parallel()
	for target, want := range map[string]bool{
		"/api/prune/run":                       false,
		"/api/prune/run?accept_baseline=true":  true,
		"/api/prune/run?accept_baseline=false": false,
	} {
		fake := &fakePruner{}
		s := &Server{prune: fake}
		pruneRequest(t, s.handlePruneRun, http.MethodPost, target)
		if fake.opts.AcceptBaseline != want {
			t.Fatalf("%s: AcceptBaseline=%v", target, fake.opts.AcceptBaseline)
		}
	}
}

func TestPruneRunAgainstManager(t *testing.T) {
	t.Parallel()
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		mgr, _ := managertest.New(t, nil)
		s := &Server{prune: mgr}
		response := pruneRequest(t, s.handlePruneRun, http.MethodPost, "/api/prune/run")
		if response.Code != http.StatusConflict {
			t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
		}
	})
	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		mgr, _ := managertest.New(t, func(cfg *config.Config) {
			cfg.Prune = config.Prune{
				Enabled: true, Mode: config.PruneModeReport, Schedule: "1h", Grace: "168h", MaxPerRun: 100,
				Roots: []string{root},
			}
		})
		s := &Server{prune: mgr}
		response := pruneRequest(t, s.handlePruneRun, http.MethodPost, "/api/prune/run")
		body := decodeReport(t, response)
		if response.Code != http.StatusOK || body["started_at"] == nil || body["aborted"] == "" {
			t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestPruneRoutesRequireAuthentication(t *testing.T) {
	t.Parallel()
	downloads := t.TempDir()
	store := managertest.Store(t, func(cfg *config.Config) {
		cfg.DownloadFolder = downloads
		cfg.Debrids = []config.Debrid{{Name: "realdebrid", APIKey: "key"}}
	})
	if err := store.Get().SaveAuth(&config.Auth{APIToken: "token", TokenOnly: true}); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, store)
	server.cookie = sessions.NewCookieStore([]byte("test-secret"))
	handler := server.WebRoutes()
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/api/prune/status"},
		{http.MethodPost, "/api/prune/run"},
		{http.MethodPost, "/api/prune/run?accept_baseline=true"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status = %d", test.method, test.path, response.Code)
		}
	}
}
