package realdebrid

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

type pollServer struct {
	gets, posts atomic.Int32
	url         string
}

func servePoll(t *testing.T, status string, selected int) *pollServer {
	t.Helper()
	ps := &pollServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ps.posts.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ps.gets.Add(1)
		fmt.Fprintf(
			w,
			`{"id":"id","status":%q,"filename":"movie","files":[{"id":1,"path":"/movie.mkv","bytes":1000,"selected":%d}],"links":["https://example.test/movie"]}`,
			status,
			selected,
		)
	}))
	t.Cleanup(server.Close)
	ps.url = server.URL
	return ps
}

func TestPollSelectsOncePerCallWhenAllowed(t *testing.T) {
	t.Parallel()
	ps := servePoll(t, "waiting_files_selection", 0)
	provider := testRealDebrid(ps.url)
	for call := 1; call <= 2; call++ {
		_, obs, err := provider.Poll(t.Context(), &types.Torrent{ID: "id"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Phase != types.PhaseAwaitingSelection || !obs.Nudged {
			t.Fatalf("call %d: phase %s nudged %v, want awaiting_selection nudged", call, obs.Phase, obs.Nudged)
		}
		if got := ps.posts.Load(); got != int32(call) {
			t.Fatalf("after call %d: %d select requests, want %d", call, got, call)
		}
		if got := ps.gets.Load(); got != int32(call) {
			t.Fatalf("after call %d: %d status requests, want %d", call, got, call)
		}
	}
}

func TestPollSendsNoSelectWhenNotAllowed(t *testing.T) {
	t.Parallel()
	ps := servePoll(t, "waiting_files_selection", 0)
	_, obs, err := testRealDebrid(ps.url).Poll(t.Context(), &types.Torrent{ID: "id"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Phase != types.PhaseAwaitingSelection || obs.Nudged {
		t.Fatalf("phase %s nudged %v, want awaiting_selection not nudged", obs.Phase, obs.Nudged)
	}
	if got := ps.posts.Load(); got != 0 {
		t.Fatalf("%d select requests, want none", got)
	}
}

func TestPollMakesOneFetchPerRawState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state    string
		selected int
		phase    types.Phase
		cached   types.Verdict
		files    int
	}{
		{"magnet_conversion", 0, types.PhaseConverting, types.CachedUnknown, 0},
		{"queued", 0, types.PhaseRemoteQueued, types.CachedNo, 0},
		{"downloading", 0, types.PhaseDownloading, types.CachedNo, 0},
		{"compressing", 0, types.PhaseDownloading, types.CachedNo, 0},
		{"uploading", 0, types.PhaseDownloading, types.CachedNo, 0},
		{"downloaded", 1, types.PhaseDownloaded, types.CachedUnknown, 1},
		{"error", 0, types.PhaseFailed, types.CachedUnknown, 0},
		{"virus", 0, types.PhaseFailed, types.CachedUnknown, 0},
		{"future_state", 0, types.PhaseStalled, types.CachedUnknown, 0},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			ps := servePoll(t, tc.state, tc.selected)
			got, obs, err := testRealDebrid(ps.url).Poll(t.Context(), &types.Torrent{ID: "id"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Phase != tc.phase || obs.Cached != tc.cached || obs.Raw != tc.state || obs.ID != "id" ||
				obs.Provider != "realdebrid" {
				t.Fatalf("observation = %+v, want phase %s cached %d", obs, tc.phase, tc.cached)
			}
			if len(got.Files) != tc.files {
				t.Fatalf("files = %d, want %d", len(got.Files), tc.files)
			}
			if gets, posts := ps.gets.Load(), ps.posts.Load(); gets != 1 || posts != 0 {
				t.Fatalf("requests: %d status, %d select; want 1 and 0", gets, posts)
			}
		})
	}
}

func TestPollKeepsTheRequestErrors(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	_, _, err := testRealDebrid(server.URL).Poll(t.Context(), &types.Torrent{ID: "id", Name: "n"}, true)
	if !errors.Is(err, customerror.ErrTorrentNotFound) {
		t.Fatalf("error = %v, want ErrTorrentNotFound", err)
	}
}
