package premiumize

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestLookup(t *testing.T) {
	t.Parallel()
	const match = `{"id":"tr1","name":"Name","status":"running","src":"magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01&dn=x"}`
	tests := []struct {
		name   string
		status int
		body   string
		cancel bool
		want   types.LookupResult
	}{
		{
			name:   "matching src running",
			status: http.StatusOK,
			body:   `{"status":"success","transfers":[` + match + `]}`,
			want:   types.LookupFound,
		},
		{
			name:   "no match",
			status: http.StatusOK,
			body:   `{"status":"success","transfers":[{"id":"x","src":"magnet:?xt=urn:btih:1111111111111111111111111111111111111111"}]}`,
			want:   types.LookupUnknown,
		},
		{
			name:   "no src",
			status: http.StatusOK,
			body:   `{"status":"success","transfers":[{"id":"x","name":"n","status":"finished"}]}`,
			want:   types.LookupUnknown,
		},
		{
			name:   "empty list",
			status: http.StatusOK,
			body:   `{"status":"success","transfers":[]}`,
			want:   types.LookupUnknown,
		},
		{name: "http error", status: http.StatusInternalServerError, body: `{}`, want: types.LookupUnknown},
		{
			name:   "error envelope",
			status: http.StatusOK,
			body:   `{"status":"error","message":"bad","code":"x"}`,
			want:   types.LookupUnknown,
		},
		{
			name:   "cancelled",
			status: http.StatusOK,
			body:   `{"status":"success","transfers":[` + match + `]}`,
			cancel: true,
			want:   types.LookupUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(server.Close)
			pm := &Premiumize{
				Host:   server.URL,
				client: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
				config: config.Debrid{Name: "premiumize"},
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			got, torrent := pm.Lookup(ctx, "abcdef0123456789abcdef0123456789abcdef01")
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
	if torrent.ID != "tr1" || torrent.Debrid != "premiumize" || torrent.Name != "Name" ||
		torrent.OriginalFilename != "Name" || torrent.Status != types.TorrentStatusDownloading ||
		!strings.EqualFold(torrent.InfoHash, "abcdef0123456789abcdef0123456789abcdef01") {
		t.Fatalf("torrent = %#v", torrent)
	}
}
