package torbox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func serveTorboxState(t *testing.T, state string, finished bool) (*atomic.Int32, string) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(
			w,
			`{"success":true,"data":{"id":17,"name":"Release.mkv","size":100,"progress":0,"download_state":%q,"download_finished":%t,"hash":"ABC","files":[{"id":1,"name":"Release.mkv","absolute_path":"Release.mkv","size":100}]}}`,
			state,
			finished,
		)
	}))
	t.Cleanup(server.Close)
	return &requests, server.URL
}

func TestPollMakesOneRequestPerRawState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state    string
		finished bool
		phase    types.Phase
		cached   types.Verdict
		files    int
	}{
		{"downloading", false, types.PhaseDownloading, types.CachedNo, 1},
		{"queued", false, types.PhaseRemoteQueued, types.CachedNo, 1},
		{"metaDL", false, types.PhaseDownloading, types.CachedNo, 1},
		{"stalled", false, types.PhaseStalled, types.CachedUnknown, 1},
		{"cached", false, types.PhaseFinalizing, types.CachedUnknown, 1},
		{"uploading", true, types.PhaseDownloaded, types.CachedUnknown, 1},
		{"future_state", false, types.PhaseStalled, types.CachedUnknown, 1},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			requests, url := serveTorboxState(t, tc.state, tc.finished)
			got, obs, err := testTorbox(url).Poll(t.Context(), &types.Torrent{ID: "17"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Phase != tc.phase || obs.Cached != tc.cached || obs.ID != "17" || obs.Provider != "torbox" {
				t.Fatalf("observation = %+v, want phase %s cached %d", obs, tc.phase, tc.cached)
			}
			if len(got.Files) != tc.files {
				t.Fatalf("files = %d, want %d", len(got.Files), tc.files)
			}
			if tc.phase == types.PhaseDownloaded {
				requireLinks(t, got.Files)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("%d requests, want 1", got)
			}
		})
	}
}

func requireLinks(t *testing.T, files map[string]types.File) {
	t.Helper()
	for _, f := range files {
		if f.Link == "" {
			t.Fatalf("downloaded file has no link: %+v", f)
		}
	}
}

func TestPollKeepsAStalledTorrentOutOfTheErrorStatus(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"stalled", "stalledDL", "future_state"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			_, url := serveTorboxState(t, state, false)
			got, obs, err := testTorbox(url).Poll(t.Context(), &types.Torrent{ID: "17"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Phase != types.PhaseStalled || obs.Cached == types.CachedNo {
				t.Fatalf("observation = %+v, want a stalled one that is not known uncached", obs)
			}
			if got.Status != types.TorrentStatusDownloading {
				t.Fatalf("status = %s, want downloading so a stall is not a dead release", got.Status)
			}
		})
	}
}
