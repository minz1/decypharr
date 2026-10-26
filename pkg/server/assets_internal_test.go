package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

func assetServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	s := newTestServer(t, managertest.Store(t, nil))
	return s.WebRoutes(), s.assets.version
}

func getAsset(h http.Handler, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAssetWithCurrentVersionIsImmutable(t *testing.T) {
	t.Parallel()
	h, version := assetServer(t)
	w := getAsset(h, "/assets/js/common.js?v="+version, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestAssetWithoutVersionRevalidates(t *testing.T) {
	t.Parallel()
	h, _ := assetServer(t)
	for _, target := range []string{"/assets/js/common.js", "/assets/js/common.js?v=stale"} {
		w := getAsset(h, target, nil)
		etag := w.Header().Get("ETag")
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-cache" || etag == "" {
			t.Fatalf("%s: status %d, Cache-Control %q, ETag %q", target, w.Code, w.Header().Get("Cache-Control"), etag)
		}
		again := getAsset(h, target, map[string]string{"If-None-Match": etag})
		if again.Code != http.StatusNotModified {
			t.Fatalf("%s: conditional status = %d, want 304", target, again.Code)
		}
	}
}

func TestAssetIsGzipped(t *testing.T) {
	t.Parallel()
	h, _ := assetServer(t)
	plain := getAsset(h, "/assets/js/common.js", nil)
	w := getAsset(h, "/assets/js/common.js", map[string]string{"Accept-Encoding": "gzip"})
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", w.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain.Body.Bytes()) {
		t.Fatal("gunzipped body differs from the plain asset")
	}
}

func TestPageReferencesVersionedAssets(t *testing.T) {
	t.Parallel()
	h, version := assetServer(t)
	w := getAsset(h, "/setup", map[string]string{"Accept-Encoding": "gzip"})
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("status %d, Content-Encoding %q: %v", w.Code, w.Header().Get("Content-Encoding"), err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	if !strings.Contains(page, "images/favicon/favicon-32x32.png?v="+version) {
		t.Fatalf("page does not reference versioned assets:\n%s", page)
	}
	if strings.Contains(strings.ToLower(page), "jquery") {
		t.Fatal("page still loads jquery")
	}
}

func TestEveryTemplateAssetReferenceIsVersioned(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(content, "templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		src, readErr := fs.ReadFile(content, "templates/"+e.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if (strings.Contains(line, "assets/") || strings.Contains(line, "images/")) &&
				!strings.Contains(line, "?v={{.AssetVersion}}") {
				t.Errorf("%s:%d: asset reference without ?v=: %s", e.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The navbar logo is on every page; it must not be fetched again on each click.
func TestImageWithCurrentVersionIsImmutable(t *testing.T) {
	t.Parallel()
	h, version := assetServer(t)
	w := getAsset(h, "/images/logo.png?v="+version, nil)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("status = %d, Cache-Control = %q", w.Code, w.Header().Get("Cache-Control"))
	}
	again := getAsset(h, "/images/logo.png", map[string]string{"If-None-Match": w.Header().Get("ETag")})
	if w.Header().Get("ETag") == "" || again.Code != http.StatusNotModified {
		t.Fatalf("ETag = %q, revalidation status = %d, want 304", w.Header().Get("ETag"), again.Code)
	}
}

// A missing file must not be cached for a year under the current version.
func TestMissingAssetIsNotCachedForever(t *testing.T) {
	t.Parallel()
	h, version := assetServer(t)
	w := getAsset(h, "/assets/js/missing.js?v="+version, nil)
	if w.Code != http.StatusNotFound || strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("status = %d, Cache-Control = %q", w.Code, w.Header().Get("Cache-Control"))
	}
}

// Range would make the compressor gzip a partial body under a plain Content-Range; assets are served whole.
func TestAssetIgnoresRange(t *testing.T) {
	t.Parallel()
	h, version := assetServer(t)
	full := getAsset(h, "/assets/js/common.js?v="+version, nil)
	w := getAsset(h, "/assets/js/common.js?v="+version, map[string]string{"Range": "bytes=0-9"})
	if w.Code != http.StatusOK || w.Body.Len() != full.Body.Len() {
		t.Fatalf("status = %d, %d bytes, want 200 and %d bytes", w.Code, w.Body.Len(), full.Body.Len())
	}
}
