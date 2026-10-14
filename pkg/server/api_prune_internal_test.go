package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/sessions"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

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

func pruneManager(t *testing.T, root string) (*manager.Manager, *Server, string) {
	t.Helper()
	mount := filepath.Join(t.TempDir(), "mnt")
	mgr, _ := managertest.New(t, func(cfg *config.Config) {
		cfg.Mount.MountPath = mount
		cfg.Prune = config.Prune{
			Enabled: true, Mode: config.PruneModeReport, Schedule: "1h", Grace: "168h", MaxPerRun: 100,
			Roots: []string{root},
		}
	})
	return mgr, &Server{manager: mgr}, mount
}

func TestPruneStatusReturnsLastReport(t *testing.T) {
	t.Parallel()
	_, s, _ := pruneManager(t, t.TempDir())
	run := pruneRequest(t, s.handlePruneRun, http.MethodPost, "/api/prune/run")
	if run.Code != http.StatusOK {
		t.Fatalf("run code=%d", run.Code)
	}

	response := pruneRequest(t, s.handlePruneStatus, http.MethodGet, "/api/prune/status")

	body := decodeReport(t, response)
	if response.Code != http.StatusOK || body["started_at"] == nil || body["aborted"] == "" {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPruneRunPassesAcceptBaseline(t *testing.T) {
	t.Parallel()
	for target, want := range map[string]bool{
		"/api/prune/run":                       false,
		"/api/prune/run?accept_baseline=true":  true,
		"/api/prune/run?accept_baseline=false": false,
	} {
		root := t.TempDir()
		mgr, s, mount := pruneManager(t, root)
		entry := &storage.Entry{
			InfoHash: "aaaa", Name: "Show.aaaa", Size: 1, IsComplete: true,
			Files: map[string]*storage.File{"a.mkv": {Name: "a.mkv", Size: 1, InfoHash: "aaaa"}},
		}
		if err := mgr.Storage().AddOrUpdate(entry); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Storage().SetLastReferencedCount(100); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(mount, "__all__", "Show.aaaa", "a.mkv")
		if err := os.Symlink(dest, filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}

		body := decodeReport(t, pruneRequest(t, s.handlePruneRun, http.MethodPost, target))

		if body["accepted_baseline"] != want || body["referenced"] != float64(1) {
			t.Fatalf("%s: body=%v", target, body)
		}
	}
}

func TestPruneRunDisabledIsConflict(t *testing.T) {
	t.Parallel()
	mgr, _ := managertest.New(t, nil)
	s := &Server{manager: mgr}
	response := pruneRequest(t, s.handlePruneRun, http.MethodPost, "/api/prune/run")
	if response.Code != http.StatusConflict || decodeReport(t, response)["error"] == nil {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
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
