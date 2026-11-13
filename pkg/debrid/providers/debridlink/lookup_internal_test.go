package debridlink

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestLookup(t *testing.T) {
	t.Parallel()
	const entry = `{"id":"t7","hashString":"ABCDEF","name":"Name","status":4}`
	const other = `{"id":"t8","hashString":"zzz","name":"Other","status":100}`
	tests := []struct {
		name   string
		pages  []string
		failAt int
		cancel bool
		want   types.LookupResult
	}{
		{name: "found unfinished mixed case", pages: []string{other, entry}, want: types.LookupFound},
		{name: "absent", pages: []string{other}, want: types.LookupAbsent},
		{name: "empty account", pages: nil, want: types.LookupAbsent},
		{name: "second page fails", pages: []string{other, other}, failAt: 1, want: types.LookupUnknown},
		{name: "first page fails", pages: []string{other}, failAt: 0, want: types.LookupUnknown},
		{name: "cancelled", pages: []string{entry}, failAt: -1, cancel: true, want: types.LookupUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if tc.want == types.LookupUnknown && !tc.cancel && page >= tc.failAt {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				body := "[]"
				if page < len(tc.pages) {
					body = "[" + tc.pages[page] + "]"
				}
				_, _ = fmt.Fprintf(w, `{"success":true,"value":%s}`, body)
			}))
			t.Cleanup(server.Close)
			provider, err := New(config.Debrid{Name: "debridlink", APIKey: "token"}, nil, mkvOnly())
			if err != nil {
				t.Fatal(err)
			}
			provider.Host = server.URL
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			got, torrent := provider.Lookup(ctx, "abcdef")
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
	if torrent.ID != "t7" || torrent.Debrid != "debridlink" || torrent.InfoHash != "ABCDEF" ||
		torrent.Name != "Name" || torrent.OriginalFilename != "Name" || torrent.Status != types.TorrentStatusDownloading {
		t.Fatalf("torrent = %#v", torrent)
	}
}
