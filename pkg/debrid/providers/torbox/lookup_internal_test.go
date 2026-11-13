package torbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestLookup(t *testing.T) {
	t.Parallel()
	const entry = `{"id":7,"hash":"ABCDEF","name":"Name","download_state":"downloading","download_finished":false}`
	const other = `{"id":8,"hash":"zzz","name":"Other","download_state":"completed","download_finished":true}`
	const queuedOK = `{"success":true,"data":[]}`
	tests := []struct {
		name   string
		pages  []string
		failAt int
		queued string
		cancel bool
		want   types.LookupResult
	}{
		{
			name:   "found downloading mixed case",
			pages:  []string{other + "," + entry},
			queued: queuedOK,
			want:   types.LookupFound,
		},
		{name: "found on later page", pages: []string{other, entry}, queued: queuedOK, want: types.LookupFound},
		{name: "absent", pages: []string{other}, queued: queuedOK, want: types.LookupAbsent},
		{name: "second page fails", pages: []string{other}, failAt: 2, queued: queuedOK, want: types.LookupUnknown},
		{name: "first page fails", pages: []string{other}, failAt: 1, queued: queuedOK, want: types.LookupUnknown},
		{
			name:   "queued only",
			pages:  []string{other},
			queued: `{"success":true,"data":[{"id":99,"hash":"abcdef"}]}`,
			want:   types.LookupUnknown,
		},
		{
			name:   "queued listing fails",
			pages:  []string{other},
			queued: `{"success":false,"error":"x"}`,
			want:   types.LookupUnknown,
		},
		{name: "cancelled", pages: []string{entry}, queued: queuedOK, cancel: true, want: types.LookupUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(lookupHandler(t, tc.pages, tc.failAt, tc.queued))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			got, torrent := testTorbox(server.URL).Lookup(ctx, "abcdef")
			checkLookup(t, got, torrent, tc.want)
		})
	}
}

func checkLookup(t *testing.T, got types.LookupResult, torrent *types.Torrent, want types.LookupResult) {
	t.Helper()
	if got != want {
		t.Fatalf("result = %v, want %v", got, want)
	}
	if want != types.LookupFound {
		if torrent != nil {
			t.Fatalf("torrent = %#v", torrent)
		}
		return
	}
	if torrent.ID != "7" || torrent.Debrid != "torbox" || torrent.InfoHash != "ABCDEF" ||
		torrent.Name != "Name" || torrent.OriginalFilename != "Name" || torrent.Status != types.TorrentStatusDownloading {
		t.Fatalf("torrent = %#v", torrent)
	}
}

func lookupHandler(t *testing.T, pages []string, failAt int, queued string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/queued/getqueued" {
			_, _ = fmt.Fprint(w, queued)
			return
		}
		if r.URL.Query().Get("bypass_cache") != "true" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page := offset + 1
		if failAt != 0 && page >= failAt {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body := "[]"
		if offset < len(pages) {
			body = "[" + pages[offset] + "]"
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"data":%s}`, body)
	}
}
