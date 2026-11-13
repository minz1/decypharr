package premiumize

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestCheckStatusVerdictPerRawState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		state      string
		unlinked   bool
		uncached   bool
		wantStatus types.TorrentStatus
		wantErr    error
		wantText   string
	}{
		{name: "finished", state: "finished", wantStatus: types.TorrentStatusDownloaded},
		{name: "seeding", state: "seeding", wantStatus: types.TorrentStatusDownloaded},
		{name: "finished in capitals", state: "FINISHED", wantStatus: types.TorrentStatusDownloaded},
		{
			name:       "finished without links",
			state:      "finished",
			unlinked:   true,
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrProviderStatusTimeout,
		},
		{
			name:       "seeding without links",
			state:      "seeding",
			unlinked:   true,
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrProviderStatusTimeout,
		},
		{
			name:       "queued",
			state:      "queued",
			wantStatus: types.TorrentStatusRemoteQueued,
			wantErr:    customerror.ErrTorrentNotCached,
		},
		{name: "queued uncached allowed", state: "queued", uncached: true, wantStatus: types.TorrentStatusRemoteQueued},
		{
			name:       "running",
			state:      "running",
			wantStatus: types.TorrentStatusDownloading,
			wantErr:    customerror.ErrTorrentNotCached,
		},
		{
			name:       "running uncached allowed",
			state:      "running",
			uncached:   true,
			wantStatus: types.TorrentStatusDownloading,
		},
		{
			name:       "waiting is an error today",
			state:      "waiting",
			wantStatus: types.TorrentStatusError,
			wantText:   "has error status",
		},
		{name: "error", state: "error", wantStatus: types.TorrentStatusError, wantText: "has error status"},
		{name: "deleted", state: "deleted", wantStatus: types.TorrentStatusError, wantText: "has error status"},
		{name: "banned", state: "banned", wantStatus: types.TorrentStatusError, wantText: "has error status"},
		{name: "timeout", state: "timeout", wantStatus: types.TorrentStatusError, wantText: "has error status"},
		{
			name:       "unknown string",
			state:      "future_state",
			wantStatus: types.TorrentStatusError,
			wantText:   "has error status",
		},
		{name: "empty string", state: "", wantStatus: types.TorrentStatusError, wantText: "has error status"},
	}
	for i := range cases {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/transfer/list", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(
					w,
					`{"status":"success","transfers":[{"id":"t1","name":"Release","status":%q,"progress":1,"folder_id":"f1","file_id":null}]}`,
					tc.state,
				)
			})
			mux.HandleFunc("GET /api/folder/list", func(w http.ResponseWriter, _ *http.Request) {
				link := "https://example.test/movie.mkv"
				if tc.unlinked {
					link = ""
				}
				fmt.Fprintf(
					w,
					`{"status":"success","folder_id":"f1","content":[{"id":"file-1","name":"movie.mkv","type":"file","size":1024,"link":%q}]}`,
					link,
				)
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			pm := &Premiumize{
				Host:               server.URL,
				client:             request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
				config:             config.Debrid{Name: "premiumize"},
				logger:             zerolog.Nop(),
				options:            types.ProviderOptions{ValidateFile: func(string, int64) error { return nil }},
				pollInterval:       time.Millisecond,
				maxFinalizingPolls: 1,
			}
			got, err := pm.CheckStatus(t.Context(), &types.Torrent{ID: "t1", DownloadUncached: tc.uncached})
			checkVerdict(t, err, tc.wantErr, tc.wantText)
			if got == nil || got.Status != tc.wantStatus {
				t.Fatalf("result = %#v, want status %s", got, tc.wantStatus)
			}
		})
	}
}

func TestObserveMapsEveryRawState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state  string
		linked bool
		phase  types.Phase
		stall  types.Phase
	}{
		{"finished", true, types.PhaseDownloaded, types.PhaseDownloaded},
		{"seeding", true, types.PhaseDownloaded, types.PhaseDownloaded},
		{"FINISHED", true, types.PhaseDownloaded, types.PhaseDownloaded},
		{"finished", false, types.PhaseFinalizing, types.PhaseFinalizing},
		{"seeding", false, types.PhaseFinalizing, types.PhaseFinalizing},
		{"queued", false, types.PhaseRemoteQueued, types.PhaseStalled},
		{"waiting", false, types.PhaseRemoteQueued, types.PhaseStalled},
		{"running", false, types.PhaseDownloading, types.PhaseDownloading},
		{"error", false, types.PhaseFailed, types.PhaseFailed},
		{"deleted", false, types.PhaseFailed, types.PhaseFailed},
		{"banned", false, types.PhaseFailed, types.PhaseFailed},
		{"timeout", false, types.PhaseFailed, types.PhaseFailed},
		{"future_state", false, types.PhaseUnknown, types.PhaseStalled},
		{"", false, types.PhaseUnknown, types.PhaseStalled},
	}
	for _, c := range cases {
		o := observe(c.state, c.linked)
		if o.Phase != c.phase || o.Raw != c.state {
			t.Errorf("%q linked=%t: got phase %s raw %q, want %s", c.state, c.linked, o.Phase, o.Raw, c.phase)
		}
		if o.Phase != types.PhaseUnknown && !types.LegalEdge(types.PhaseNone, o.Phase) {
			t.Errorf("%q: none -> %s is not a legal edge", c.state, o.Phase)
		}
		stalled := types.WithStall(o)
		if stalled.Phase != c.stall || stalled.Raw != c.state {
			t.Errorf(
				"%q at 0 seeders and 0 progress: got phase %s raw %q, want %s",
				c.state,
				stalled.Phase,
				stalled.Raw,
				c.stall,
			)
		}
	}
}

func checkVerdict(t *testing.T, err, wantErr error, wantText string) {
	t.Helper()
	switch {
	case wantErr != nil:
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
	case wantText != "":
		if err == nil || !strings.Contains(err.Error(), wantText) {
			t.Fatalf("error = %v, want %q", err, wantText)
		}
	case err != nil:
		t.Fatal(err)
	}
}
