package alldebrid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestLookup(t *testing.T) {
	t.Parallel()
	const listing = `{"status":"success","data":{"magnets":[{"id":7,"filename":"Name","hash":"ABCDEF","statusCode":1},{"id":8,"filename":"Other","hash":"zzz","statusCode":4}]}}`
	tests := []struct {
		name   string
		status int
		body   string
		cancel bool
		want   types.LookupResult
	}{
		{name: "found downloading mixed case", status: http.StatusOK, body: listing, want: types.LookupFound},
		{
			name:   "not listed is never absent",
			status: http.StatusOK,
			body:   `{"status":"success","data":{"magnets":[{"id":8,"hash":"zzz","statusCode":4}]}}`,
			want:   types.LookupUnknown,
		},
		{name: "http error", status: http.StatusInternalServerError, body: listing, want: types.LookupUnknown},
		{
			name:   "error envelope",
			status: http.StatusOK,
			body:   `{"status":"error","error":{"code":"AUTH_BAD_APIKEY","message":"bad"}}`,
			want:   types.LookupUnknown,
		},
		{name: "cancelled", status: http.StatusOK, body: listing, cancel: true, want: types.LookupUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Has("status") {
					t.Errorf("unexpected status filter %q", r.URL.RawQuery)
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(server.Close)
			ad := &AllDebrid{
				Host:   server.URL,
				client: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
				config: config.Debrid{Name: "alldebrid"},
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			got, torrent := ad.Lookup(ctx, "abcdef")
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
	if torrent.ID != "7" || torrent.Debrid != "alldebrid" || torrent.InfoHash != "ABCDEF" ||
		torrent.Name != "Name" || torrent.OriginalFilename != "Name" || torrent.Status != types.TorrentStatusDownloading {
		t.Fatalf("torrent = %#v", torrent)
	}
}
