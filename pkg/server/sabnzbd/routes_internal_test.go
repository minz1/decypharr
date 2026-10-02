package sabnzbd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// newContractRouter starts a manager with token-only auth ("test-token").
func newContractRouter(t *testing.T) (*SABnzbd, http.Handler, *manager.Manager) {
	t.Helper()
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	if _, err := config.Update(func(c *config.Config) error {
		c.UseAuth = true
		c.Auth = &config.Auth{APIToken: "test-token", TokenOnly: true}
		c.DownloadFolder = t.TempDir()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New()
	t.Cleanup(func() {
		if err := mgr.Stop(); err != nil {
			t.Error(err)
		}
	})
	sab := New(mgr)
	return sab, sab.Routes(), mgr
}

// sabRequest sends values in the query (GET) or as a form body (POST).
func sabRequest(method string, values url.Values) *http.Request {
	if method == http.MethodPost {
		req := httptest.NewRequest(method, "/api/", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}
	return httptest.NewRequest(method, "/api/?"+values.Encode(), nil)
}

func addQueueFixtures(t *testing.T, mgr *manager.Manager) {
	t.Helper()
	for i, tc := range []struct {
		category string
		protocol config.Protocol
		state    storage.TorrentState
	}{
		{"tv", config.ProtocolNZB, storage.EntryStateDownloading},
		{"movies", config.ProtocolNZB, storage.EntryStateDownloading},
		{"tv", config.ProtocolTorrent, storage.EntryStateDownloading},
		{"tv", config.ProtocolNZB, storage.EntryStatePausedUP},
	} {
		entry := &storage.Entry{
			InfoHash: fmt.Sprintf("entry-%d", i),
			Name:     fmt.Sprintf("Release%d.nzb", i),
			Category: tc.category,
			Protocol: tc.protocol,
			State:    tc.state,
			Size:     4 << 20,
			Progress: 0.25,
			SavePath: t.TempDir(),
		}
		if err := mgr.Queue().Add(entry); err != nil {
			t.Fatal(err)
		}
	}
}

func checkQueueSlot(t *testing.T, body []byte) {
	t.Helper()
	var got QueueResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Status || got.Version != Version || len(got.Queue.Slots) != 1 {
		t.Fatalf("queue = %#v", got)
	}
	slot := got.Queue.Slots[0]
	if slot.NzoID != "entry-0" || slot.Cat != "tv" || slot.Filename != "Release0.nzb" || slot.Mb != "4.00" ||
		slot.MBLeft != "3.00" || slot.Percentage != "25" || slot.Status != StatusDownloading || slot.Labels == nil {
		t.Fatalf("slot = %#v", slot)
	}
}

//nolint:paralleltest // mutates the process-wide config singleton
func TestRouterQueueContracts(t *testing.T) {
	_, router, mgr := newContractRouter(t)
	addQueueFixtures(t, mgr)
	for _, tc := range []struct {
		name, method, key, token string
		wantStatus               int
	}{
		{"query", http.MethodGet, "category", "test-token", 200},
		{"form", http.MethodPost, "category", "test-token", 200},
		{"category alias", http.MethodGet, "cat", "test-token", 200},
		{"form category alias", http.MethodPost, "cat", "test-token", 200},
		{"wrong token", http.MethodGet, "category", "wrong", 401},
		{"missing token", http.MethodGet, "category", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := url.Values{"mode": {"queue"}, tc.key: {"tv"}, "ma_password": {tc.token}}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, sabRequest(tc.method, values))
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if tc.wantStatus == 200 {
				checkQueueSlot(t, response.Body.Bytes())
			}
		})
	}
}

//nolint:paralleltest // mutates the process-wide config singleton
func TestRouterUnknownMode(t *testing.T) {
	_, router, _ := newContractRouter(t)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/?mode=unknown&ma_password=test-token", nil))
	if response.Code != 404 {
		t.Fatalf("status = %d", response.Code)
	}
}

//nolint:paralleltest // mutates the process-wide config singleton
func TestRouterAuthenticatedArrSurvivesModeParsing(t *testing.T) {
	sab, _, mgr := newContractRouter(t)
	uncached := true
	mgr.Arr().AddOrUpdate(arr.Arr{
		Name: "tv", Host: "https://arr.example.test", Token: "arr-token",
		Source: arr.SourceManual, DownloadUncached: &uncached,
	})
	reached := false
	handler := sab.categoryContext(
		sab.authContext(sab.modeContext(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			reached = true
			a := getArrFromContext(r.Context())
			if a.Name != "tv" || a.Host != "https://arr.example.test" || a.Source != arr.SourceManual ||
				a.DownloadUncached == nil || !*a.DownloadUncached {
				t.Errorf("Arr = %#v", a)
			}
		}))),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/api/?mode=queue&category=tv&ma_password=test-token", nil),
	)
	if !reached {
		t.Fatalf("handler rejected request: %d", response.Code)
	}
}

// addDeleteFixtures queues four failed-or-active entries named after the
// case; entry 0 is the delete target (with a directory instead of a staged
// file when its cleanup must fail).
func addDeleteFixtures(t *testing.T, mgr *manager.Manager, name string) []*storage.Entry {
	t.Helper()
	entries := make([]*storage.Entry, 4)
	for i := range entries {
		category := "delete-tv"
		if i == 1 {
			category = "delete-movies"
		}
		entries[i] = &storage.Entry{
			InfoHash: fmt.Sprintf("%s-%d", name, i),
			Name:     "Release.nzb",
			Category: category,
			Protocol: config.ProtocolNZB,
			State:    storage.EntryStateError,
			SavePath: t.TempDir(),
			Magnet:   filepath.Join(t.TempDir(), "staged.nzb"),
		}
		if i == 2 {
			entries[i].Protocol = config.ProtocolTorrent
		}
		if i == 3 {
			entries[i].State = storage.EntryStateDownloading
		}
		stageDeleteFiles(t, entries[i], name == "cleanup failure" && i == 0)
		if err := mgr.Queue().Add(entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	return entries
}

func stageDeleteFiles(t *testing.T, entry *storage.Entry, blockRemoval bool) {
	t.Helper()
	if blockRemoval {
		mustMkdir(t, entry.Magnet)
		mustWrite(t, filepath.Join(entry.Magnet, "block-removal"), "keep")
	} else {
		mustWrite(t, entry.Magnet, "staged")
	}
	if err := os.MkdirAll(entry.DownloadPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(entry.DownloadPath(), "movie.mkv"), "movie")
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkDeleted(t *testing.T, mgr *manager.Manager, entries []*storage.Entry, wantDeleted bool) {
	t.Helper()
	for i, entry := range entries {
		deleted := i == 0 && wantDeleted
		_, err := mgr.Queue().GetTorrent(entry.InfoHash)
		if deleted && err == nil || !deleted && err != nil {
			t.Errorf("entry %s, deleted=%v, error=%v", entry.InfoHash, deleted, err)
		}
		for _, path := range []string{entry.Magnet, entry.DownloadPath()} {
			_, statErr := os.Stat(path)
			if deleted && !errors.Is(statErr, os.ErrNotExist) || !deleted && statErr != nil {
				t.Errorf("path %s, deleted=%v, error=%v", path, deleted, statErr)
			}
		}
	}
}

//nolint:paralleltest // mutates the process-wide config singleton
func TestRouterQueueDelete(t *testing.T) {
	_, router, mgr := newContractRouter(t)
	for _, tc := range []struct {
		name, value, method string
		wantStatus          int
		wantDeleted         bool
	}{
		{"one", "target", http.MethodGet, 200, true},
		{"form deletion", "target", http.MethodPost, 200, true},
		{"partial", "target,missing", http.MethodGet, 200, true},
		{"all missing", "missing", http.MethodGet, 500, false},
		{"empty", "", http.MethodGet, 400, false},
		{"cleanup failure", "target", http.MethodGet, 500, false},
		{"failed category", "failed", http.MethodGet, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := addDeleteFixtures(t, mgr, tc.name)
			values := url.Values{
				"mode":        {"queue"},
				"name":        {"delete"},
				"value":       {strings.ReplaceAll(tc.value, "target", entries[0].InfoHash)},
				"category":    {"delete-tv"},
				"ma_password": {"test-token"},
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, sabRequest(tc.method, values))
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var got StatusResponse
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != (tc.wantStatus == 200) {
				t.Fatalf("response = %#v", got)
			}
			if tc.name == "partial" && !strings.Contains(got.Error, "missing") {
				t.Errorf("partial failure is absent: %#v", got)
			}
			checkDeleted(t, mgr, entries, tc.wantDeleted)
		})
	}
}
