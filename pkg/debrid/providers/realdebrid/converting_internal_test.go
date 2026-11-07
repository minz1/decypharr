package realdebrid

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

type convertingCase struct {
	name          string
	states        []string // raw Real-Debrid states served per poll; the last repeats
	allowUncached bool
	wantGets      int32
	wantSelects   int32
	wantStatus    types.TorrentStatus
	wantErr       error
	notErr        error
}

// A magnet that is still converting says nothing about whether the release is
// cached, so it must never be reported as ErrTorrentNotCached: that error makes
// the manager delete the provider torrent and tell hearsay the release is not
// cached.
func TestCheckStatusKeepsPollingWhileMagnetConverts(t *testing.T) {
	t.Parallel()
	for _, tc := range []convertingCase{
		{
			name:        "converting then cached",
			states:      []string{"magnet_conversion", "waiting_files_selection", "downloaded"},
			wantGets:    3,
			wantSelects: 1,
			wantStatus:  types.TorrentStatusDownloaded,
		},
		{
			name: "converting repeatedly then cached",
			states: []string{
				"magnet_conversion", "magnet_conversion", "magnet_conversion",
				"waiting_files_selection", "downloaded",
			},
			wantGets:    5,
			wantSelects: 1,
			wantStatus:  types.TorrentStatusDownloaded,
		},
		{
			name:        "converting then truly downloading",
			states:      []string{"magnet_conversion", "waiting_files_selection", "downloading"},
			wantGets:    3,
			wantSelects: 1,
			wantStatus:  types.TorrentStatusDownloading,
			wantErr:     customerror.ErrTorrentNotCached,
		},
		{
			name:          "converting then downloading uncached allowed",
			states:        []string{"magnet_conversion", "waiting_files_selection", "downloading"},
			allowUncached: true,
			wantGets:      3,
			wantSelects:   1,
			wantStatus:    types.TorrentStatusDownloading,
		},
		{
			name:       "converting forever hits the cap",
			states:     []string{"magnet_conversion"},
			wantGets:   4,
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrProviderStatusTimeout,
			notErr:     customerror.ErrTorrentNotCached,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

func (tc convertingCase) run(t *testing.T) {
	var gets, selects atomic.Int32
	server := httptest.NewServer(tc.handler(t, &gets, &selects))
	defer server.Close()
	provider := &RealDebrid{
		Host:               server.URL,
		client:             request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		logger:             zerolog.Nop(),
		options:            mkvOnly(),
		pollInterval:       time.Millisecond,
		maxConversionPolls: 3,
	}
	result, err := provider.CheckStatus(&types.Torrent{ID: "id", DownloadUncached: tc.allowUncached})
	assertError(t, err, tc.wantErr, "")
	if tc.notErr != nil && errors.Is(err, tc.notErr) {
		t.Fatalf("error = %v must not be %v", err, tc.notErr)
	}
	if result == nil || result.Status != tc.wantStatus {
		t.Fatalf("result = %#v, want status %s", result, tc.wantStatus)
	}
	if gets.Load() != tc.wantGets || selects.Load() != tc.wantSelects {
		t.Fatalf("GETs = %d (want %d), selections = %d (want %d)",
			gets.Load(), tc.wantGets, selects.Load(), tc.wantSelects)
	}
}

func (tc convertingCase) handler(t *testing.T, gets, selects *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /torrents/info/id":
			state := tc.states[min(int(gets.Add(1)), len(tc.states))-1]
			selected := 0
			if state == "downloaded" {
				selected = 1
			}
			fmt.Fprintf(
				w,
				`{"status":%q,"filename":"movie","files":[{"id":1,"path":"/movie.mkv","bytes":1000,"selected":%d}],`+
					`"links":["https://example.test/movie"]}`,
				state,
				selected,
			)
		case "POST /torrents/selectFiles/id":
			selects.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}
