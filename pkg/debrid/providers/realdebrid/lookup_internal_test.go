package realdebrid

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

type lookupPageSpec struct {
	status  int
	total   string
	entries []TorrentsResponse
}

func lookupServer(t *testing.T, pages []lookupPageSpec) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 {
			t.Errorf("page = %q", r.URL.Query().Get("page"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("limit") != "1000" {
			t.Errorf("limit = %q", r.URL.Query().Get("limit"))
		}
		if page > len(pages) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		spec := pages[page-1]
		if spec.status != 0 {
			w.WriteHeader(spec.status)
			return
		}
		w.Header().Set("X-Total-Count", spec.total)
		if encodeErr := json.NewEncoder(w).Encode(spec.entries); encodeErr != nil {
			t.Error(encodeErr)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func lookupProvider(host string, limit int) *RealDebrid {
	return &RealDebrid{
		Host:   host,
		client: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		config: config.Debrid{Name: "realdebrid", Limit: limit},
	}
}

func lookupEntries(n int, prefix string) []TorrentsResponse {
	out := make([]TorrentsResponse, n)
	for i := range out {
		out[i] = TorrentsResponse{
			ID:       prefix + strconv.Itoa(i),
			Hash:     prefix + strconv.Itoa(i),
			Filename: "f",
			Status:   statusDownloaded,
		}
	}
	return out
}

func TestLookup(t *testing.T) {
	t.Parallel()
	target := TorrentsResponse{ID: "ID1", Hash: "ABCDEF", Filename: "Name", Status: "magnet_conversion"}
	one := []lookupPageSpec{{total: "1", entries: []TorrentsResponse{target}}}
	full := []lookupPageSpec{{total: "2001", entries: lookupEntries(1000, "a")},
		{total: "2001", entries: lookupEntries(1000, "b")},
		{total: "2001", entries: []TorrentsResponse{target}}}
	tests := []struct {
		name   string
		pages  []lookupPageSpec
		limit  int
		cancel bool
		want   types.LookupResult
	}{
		{name: "found non-finished mixed case", pages: one, want: types.LookupFound},
		{
			name:  "absent",
			pages: []lookupPageSpec{{total: "1", entries: lookupEntries(1, "x")}},
			want:  types.LookupAbsent,
		},
		{name: "empty account", pages: nil, want: types.LookupAbsent},
		{
			name:  "second page fails",
			pages: []lookupPageSpec{{total: "2000", entries: lookupEntries(1000, "a")}, {status: 500}},
			want:  types.LookupUnknown,
		},
		{name: "first page fails", pages: []lookupPageSpec{{status: 500}}, want: types.LookupUnknown},
		{name: "cancelled", pages: one, cancel: true, want: types.LookupUnknown},
		{name: "limit does not truncate", pages: full, limit: 10, want: types.LookupFound},
		{
			name: "total changes",
			pages: []lookupPageSpec{
				{total: "1000", entries: lookupEntries(1000, "a")},
				{total: "1001", entries: lookupEntries(1, "b")},
			},
			want: types.LookupUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := lookupProvider(lookupServer(t, tc.pages).URL, tc.limit)
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
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
	if torrent.ID != "ID1" || torrent.Debrid != "realdebrid" || torrent.InfoHash != "ABCDEF" ||
		torrent.Name != "Name" || torrent.OriginalFilename != "Name" || torrent.Status != types.TorrentStatusDownloading {
		t.Fatalf("torrent = %#v", torrent)
	}
}
