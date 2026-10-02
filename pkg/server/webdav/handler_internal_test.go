package webdav

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// handleTorrentFolder passes a nil FileInfo for unknown names. HEAD used to
// dereference it (panic) and PROPFIND answered 207 with an empty listing.
func TestUnknownPathIsNotFound(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	for _, method := range []string{http.MethodHead, http.MethodGet, PROPFIND, http.MethodDelete} {
		w := httptest.NewRecorder()
		h.handler(nil, nil, w, httptest.NewRequest(method, "/__all__/missing", nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", method, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.handler(nil, nil, w, httptest.NewRequest(http.MethodOptions, "/__all__/missing", nil))
	if w.Code != http.StatusOK {
		t.Errorf("OPTIONS = %d, want 200", w.Code)
	}
}
