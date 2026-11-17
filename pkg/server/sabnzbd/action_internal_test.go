package sabnzbd

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

func TestAddActionUsesTheCategoryOverrideUnlessTheRequestNamesOne(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		DefaultDownloadAction:   config.DownloadActionSymlink,
		CategoryDownloadActions: map[string]config.DownloadAction{"bindery": config.DownloadActionDownload},
	}
	for _, tc := range []struct {
		category, requested string
		want                config.DownloadAction
	}{
		{"bindery", "", config.DownloadActionDownload},
		{"sonarr", "", config.DownloadActionSymlink},
		{"bindery", "symlink", config.DownloadActionSymlink},
		{"sonarr", "download", config.DownloadActionDownload},
		{"bindery", "copy", config.DownloadActionDownload},
		{"sonarr", "Download", config.DownloadActionSymlink},
	} {
		if got := addAction(cfg, tc.category, tc.requested); got != tc.want {
			t.Errorf("addAction(%q, %q) = %q, want %q", tc.category, tc.requested, got, tc.want)
		}
	}
}

func newActionRouter(t *testing.T) (http.Handler, *manager.Manager) {
	t.Helper()
	mgr, store := managertest.New(t, func(c *config.Config) {
		c.UseAuth = true
		c.Auth = &config.Auth{APIToken: "test-token", TokenOnly: true}
		c.DownloadFolder = t.TempDir()
		c.DefaultDownloadAction = config.DownloadActionSymlink
		c.CategoryDownloadActions = map[string]config.DownloadAction{"bindery": config.DownloadActionDownload}
		c.Usenet = config.Usenet{
			Providers:                []config.UsenetProvider{{Host: "127.0.0.1", Port: 1, MaxConnections: 1}},
			MaxConnections:           1,
			ProcessingMaxConnections: 1,
		}
	})
	return New(mgr, store, zerolog.Nop()).Routes(), mgr
}

func storedAction(t *testing.T, mgr *manager.Manager, w *httptest.ResponseRecorder) config.DownloadAction {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("add status = %d: %s", w.Code, w.Body.String())
	}
	var added AddNZBResponse
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil || len(added.NzoIDs) != 1 {
		t.Fatalf("add response %q: %v", w.Body.String(), err)
	}
	entry, err := mgr.Queue().GetTorrent(added.NzoIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	return entry.Action
}

func TestAddFileUsesTheCategoryDownloadAction(t *testing.T) {
	t.Parallel()
	for _, tc := range actionCases() {
		category, want := tc.category, tc.want
		routes, mgr := newActionRouter(t)
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range map[string]string{"mode": ModeAddFile, "cat": category, "ma_password": "test-token", "action": tc.action} {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
		part, err := mw.CreateFormFile("name", "release.nzb")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = part.Write([]byte("parsed later by the worker")); err != nil {
			t.Fatal(err)
		}
		if err = mw.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		if got := storedAction(t, mgr, w); got != want {
			t.Errorf("addfile in %s stored action %q, want %q", category, got, want)
		}
	}
}

func TestAddURLUsesTheCategoryDownloadAction(t *testing.T) {
	t.Parallel()
	indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("parsed later by the worker"))
	}))
	t.Cleanup(indexer.Close)
	for _, tc := range actionCases() {
		category, want := tc.category, tc.want
		routes, mgr := newActionRouter(t)
		values := url.Values{
			"mode": {ModeAddURL}, "cat": {category}, "ma_password": {"test-token"}, "action": {tc.action},
			"name": {indexer.URL + "/release.nzb"},
		}
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, sabRequest(http.MethodPost, values))
		if got := storedAction(t, mgr, w); got != want {
			t.Errorf("addurl in %s stored action %q, want %q", category, got, want)
		}
	}
}

type actionCase struct {
	category, action string
	want             config.DownloadAction
}

func actionCases() []actionCase {
	return []actionCase{
		{"bindery", "", config.DownloadActionDownload},
		{"sonarr", "", config.DownloadActionSymlink},
		{"bindery", "none", config.DownloadActionNone},
	}
}
