package realdebrid

import (
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

func TestCheckStatusVerdictPerRawState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		states     []string
		uncached   bool
		wantStatus types.TorrentStatus
		wantErr    error
		wantText   string
	}{
		{
			name:       "converting forever",
			states:     []string{"magnet_conversion"},
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrMagnetNotConverted,
		},
		{
			name:       "awaiting selection then downloaded",
			states:     []string{"waiting_files_selection", "downloaded"},
			wantStatus: types.TorrentStatusDownloaded,
		},
		{
			name:       "queued",
			states:     []string{"queued"},
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrTorrentNotCached,
		},
		{
			name:       "queued uncached allowed",
			states:     []string{"queued"},
			uncached:   true,
			wantStatus: types.TorrentStatusDownloading,
		},
		{
			name:       "downloading",
			states:     []string{"downloading"},
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrTorrentNotCached,
		},
		{
			name:       "downloading uncached allowed",
			states:     []string{"downloading"},
			uncached:   true,
			wantStatus: types.TorrentStatusDownloading,
		},
		{
			name:       "compressing",
			states:     []string{"compressing"},
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrTorrentNotCached,
		},
		{
			name:       "compressing uncached allowed",
			states:     []string{"compressing"},
			uncached:   true,
			wantStatus: types.TorrentStatusDownloading,
		},
		{
			name:       "uploading",
			states:     []string{"uploading"},
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrTorrentNotCached,
		},
		{
			name:       "uploading uncached allowed",
			states:     []string{"uploading"},
			uncached:   true,
			wantStatus: types.TorrentStatusDownloading,
		},
		{name: "downloaded", states: []string{"downloaded"}, wantStatus: types.TorrentStatusDownloaded},
		{
			name:       "error",
			states:     []string{"error"},
			wantStatus: types.TorrentStatusError,
			wantText:   "has error status: error",
		},
		{
			name:       "magnet_error",
			states:     []string{"magnet_error"},
			wantStatus: types.TorrentStatusError,
			wantText:   "magnet_error",
		},
		{name: "virus", states: []string{"virus"}, wantStatus: types.TorrentStatusError, wantText: "virus"},
		{name: "dead", states: []string{"dead"}, wantStatus: types.TorrentStatusError, wantText: "dead"},
		{
			name:       "D7 unknown string is kept as a downloading placement",
			states:     []string{"future_state"},
			wantStatus: types.TorrentStatusDownloading,
		},
	}
	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				state := tc.states[min(int(gets.Add(1)), len(tc.states))-1]
				selected := 0
				if state == "downloaded" {
					selected = 1
				}
				fmt.Fprintf(
					w,
					`{"status":%q,"filename":"movie","files":[{"id":1,"path":"/movie.mkv","bytes":1000,"selected":%d}],"links":["https://example.test/movie"]}`,
					state,
					selected,
				)
			}))
			defer server.Close()
			provider := &RealDebrid{
				Host:               server.URL,
				client:             request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
				logger:             zerolog.Nop(),
				options:            mkvOnly(),
				pollInterval:       time.Millisecond,
				maxConversionPolls: 1,
			}
			result, err := provider.CheckStatus(t.Context(), &types.Torrent{ID: "id", DownloadUncached: tc.uncached})
			assertError(t, err, tc.wantErr, tc.wantText)
			if result == nil || result.Status != tc.wantStatus {
				t.Fatalf("result = %#v, want status %s", result, tc.wantStatus)
			}
		})
	}
}

func TestObserveMapsEveryRawState(t *testing.T) {
	t.Parallel()
	links := []string{"https://example.test/movie"}
	cases := []struct {
		name  string
		data  torrentInfo
		phase types.Phase
		stall types.Phase
	}{
		{"magnet_conversion", torrentInfo{Status: "magnet_conversion"}, types.PhaseConverting, types.PhaseConverting},
		{
			"waiting_files_selection",
			torrentInfo{Status: "waiting_files_selection"},
			types.PhaseAwaitingSelection,
			types.PhaseAwaitingSelection,
		},
		{"queued", torrentInfo{Status: "queued"}, types.PhaseRemoteQueued, types.PhaseRemoteQueued},
		{"downloading", torrentInfo{Status: "downloading"}, types.PhaseDownloading, types.PhaseDownloading},
		{"compressing", torrentInfo{Status: "compressing"}, types.PhaseDownloading, types.PhaseDownloading},
		{"uploading", torrentInfo{Status: "uploading"}, types.PhaseDownloading, types.PhaseDownloading},
		{"downloaded", torrentInfo{Status: "downloaded", Links: links}, types.PhaseDownloaded, types.PhaseDownloaded},
		{"downloaded without links", torrentInfo{Status: "downloaded"}, types.PhaseFinalizing, types.PhaseFinalizing},
		{"error", torrentInfo{Status: "error"}, types.PhaseFailed, types.PhaseFailed},
		{"magnet_error", torrentInfo{Status: "magnet_error"}, types.PhaseFailed, types.PhaseFailed},
		{"virus", torrentInfo{Status: "virus"}, types.PhaseFailed, types.PhaseFailed},
		{"dead", torrentInfo{Status: "dead"}, types.PhaseFailed, types.PhaseFailed},
		{"unknown string", torrentInfo{Status: "future_state"}, types.PhaseUnknown, types.PhaseStalled},
	}
	for i := range cases {
		c := &cases[i]
		o := observe(c.data)
		if o.Phase != c.phase || o.Raw != c.data.Status {
			t.Errorf("%s: got phase %s raw %q, want %s %q", c.name, o.Phase, o.Raw, c.phase, c.data.Status)
		}
		if o.Phase != types.PhaseUnknown && !types.LegalEdge(types.PhaseNone, o.Phase) {
			t.Errorf("%s: none -> %s is not a legal edge", c.name, o.Phase)
		}
		stalled := types.WithStall(o)
		if stalled.Phase != c.stall || stalled.Raw != c.data.Status {
			t.Errorf("%s at 0 seeders and 0 progress: got phase %s raw %q, want %s %q",
				c.name, stalled.Phase, stalled.Raw, c.stall, c.data.Status)
		}
	}
}
