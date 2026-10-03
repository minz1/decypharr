package webdav

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// Generated files (version.txt) are served as nosniff attachments, with
// range and conditional-request support.
func TestHandleDownloadServesGeneratedContent(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, nil)
	h := NewHandler(mgr, store, zerolog.Nop())
	info, _ := mgr.GetEntryChildren("version.txt")
	if info == nil || info.IsRemote() {
		t.Fatalf("version.txt = %+v, want generated content", info)
	}
	content := string(info.Content())

	w := httptest.NewRecorder()
	h.handleDownload(info, w, httptest.NewRequest(http.MethodGet, "/version.txt", nil))
	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != content {
		t.Fatalf("GET = %d %q, want 200 %q", resp.StatusCode, body, content)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("headers = %v", resp.Header)
	}

	ranged := httptest.NewRequest(http.MethodGet, "/version.txt", nil)
	ranged.Header.Set("Range", "bytes=0-1")
	w = httptest.NewRecorder()
	h.handleDownload(info, w, ranged)
	if w.Code != http.StatusPartialContent || w.Body.String() != content[:2] {
		t.Fatalf("range GET = %d %q, want 206 %q", w.Code, w.Body.String(), content[:2])
	}

	conditional := httptest.NewRequest(http.MethodGet, "/version.txt", nil)
	conditional.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	w = httptest.NewRecorder()
	h.handleDownload(info, w, conditional)
	if w.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", w.Code)
	}
}
