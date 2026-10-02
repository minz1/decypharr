package qbit

import (
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func newQueueTestQBit(t *testing.T, hashes ...string) *QBit {
	t.Helper()
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	config.Get().UseAuth = false
	mgr := manager.New()
	t.Cleanup(func() { _ = mgr.Stop() })
	for _, hash := range hashes {
		entry := &storage.Entry{InfoHash: hash, Name: hash, Category: "sonarr", Protocol: config.ProtocolTorrent}
		if err := mgr.Queue().Add(entry); err != nil {
			t.Fatal(err)
		}
	}
	return New(mgr)
}

func postForm(handler http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// Before the fix setCategory ignored the hashes and recategorized every
// queued entry, and a pipe-separated hash list was treated as one hash.
//
//nolint:paralleltest // mutates the process-wide config singleton
func TestSetCategoryOnlyTouchesRequestedHashes(t *testing.T) {
	q := newQueueTestQBit(t, "aaa", "bbb", "ccc")
	w := postForm(q.Routes(), "/torrents/setCategory", url.Values{"hashes": {"aaa|bbb"}, "category": {"radarr"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	for hash, want := range map[string]string{"aaa": "radarr", "bbb": "radarr", "ccc": "sonarr"} {
		entry, err := q.manager.Queue().GetTorrent(hash)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Category != want {
			t.Errorf("%s category = %q, want %q", hash, entry.Category, want)
		}
	}

	w = postForm(q.Routes(), "/torrents/setCategory", url.Values{"category": {"lidarr"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no hashes: status = %d, want 400", w.Code)
	}
	if entry, _ := q.manager.Queue().GetTorrent("ccc"); entry.Category != "sonarr" {
		t.Fatalf("request without hashes recategorized ccc to %q", entry.Category)
	}
}

// Arr clients hit these concurrently; run with -race.
//
//nolint:paralleltest // mutates the process-wide config singleton
func TestCategoriesAndTagsAreConcurrencySafe(t *testing.T) {
	q := newQueueTestQBit(t)
	// Spare capacity: an aliased append would write into the config's array.
	cfgCategories := append(make([]string, 0, 64), "sonarr")
	config.Get().Categories = cfgCategories
	routes := New(q.manager).Routes()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			postForm(routes, "/torrents/createCategory", url.Values{"category": {"x"}})
			postForm(routes, "/torrents/createTags", url.Values{"tags": {"a,b"}})
			routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/torrents/categories", nil))
			routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/torrents/tags", nil))
		})
	}
	wg.Wait()
	if got := cfgCategories[:cap(cfgCategories)][1]; got != "" {
		t.Fatalf("createCategory wrote %q into the config's category array", got)
	}
}

// categoryContext parses multipart forms before authentication; file parts
// spill to temp files with no limit of their own, so only the body cap bounds
// them. Pre-fix the whole body was consumed and the trailing field honored.
//
//nolint:paralleltest // mutates the process-wide config singleton
func TestOversizedBodyIsNotConsumed(t *testing.T) {
	q := newQueueTestQBit(t)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, _ := mw.CreateFormFile("torrents", "big.torrent")
		_, _ = io.CopyN(part, zeros{}, maxRequestBody+1)
		_ = mw.WriteField("category", "x")
		_ = mw.Close()
		_ = pw.Close()
	}()
	req := httptest.NewRequest(http.MethodPost, "/torrents/createCategory", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	q.Routes().ServeHTTP(w, req)
	_ = pr.Close()
	if w.Code == http.StatusOK {
		t.Fatal("a body over the cap was read to the end")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
