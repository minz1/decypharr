package alldebrid

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

type pollCounts struct {
	status, restarts atomic.Int32
	url              string
}

func servePollCode(t *testing.T, code int) *pollCounts {
	t.Helper()
	counts := &pollCounts{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /magnet/status", func(w http.ResponseWriter, _ *http.Request) {
		counts.status.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(
			w,
			`{"status":"success","data":{"magnets":[{"id":42,"filename":"Release.mkv","statusCode":%d}]}}`,
			code,
		)
	})
	mux.HandleFunc("POST /magnet/restart", func(w http.ResponseWriter, _ *http.Request) {
		counts.restarts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"message":"ok"}}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	counts.url = server.URL
	return counts
}

func TestPollMakesOneStatusRequestPerRawCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code   int
		phase  debridTypes.Phase
		cached debridTypes.Verdict
	}{
		{0, debridTypes.PhaseRemoteQueued, debridTypes.CachedNo},
		{1, debridTypes.PhaseDownloading, debridTypes.CachedNo},
		{3, debridTypes.PhaseDownloading, debridTypes.CachedNo},
		{4, debridTypes.PhaseDownloaded, debridTypes.CachedUnknown},
		{5, debridTypes.PhaseFailed, debridTypes.CachedUnknown},
		{10, debridTypes.PhaseFailed, debridTypes.CachedUnknown},
		{-1, debridTypes.PhaseStalled, debridTypes.CachedUnknown},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			t.Parallel()
			counts := servePollCode(t, tc.code)
			_, obs, err := testAllDebrid(counts.url).Poll(t.Context(), &debridTypes.Torrent{ID: "42"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Phase != tc.phase || obs.Cached != tc.cached || obs.ID != "42" || obs.Provider != "alldebrid" {
				t.Fatalf("observation = %+v, want phase %s cached %d", obs, tc.phase, tc.cached)
			}
			if got := counts.status.Load(); got != 1 {
				t.Fatalf("%d status requests, want 1", got)
			}
			if got := counts.restarts.Load(); got != 0 {
				t.Fatalf("%d restarts, want none", got)
			}
		})
	}
}

func TestPollRestartsStatusSevenWhileAllowedThenFails(t *testing.T) {
	t.Parallel()
	counts := servePollCode(t, 7)
	ad := testAllDebrid(counts.url)
	_, obs, err := ad.Poll(t.Context(), &debridTypes.Torrent{ID: "42"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Phase != debridTypes.PhaseStalled || !obs.Nudged {
		t.Fatalf("allowed: phase %s nudged %v, want stalled nudged", obs.Phase, obs.Nudged)
	}
	if status, restarts := counts.status.Load(), counts.restarts.Load(); status != 1 || restarts != 1 {
		t.Fatalf("allowed: %d status, %d restarts; want 1 and 1", status, restarts)
	}
	_, obs, err = ad.Poll(t.Context(), &debridTypes.Torrent{ID: "42"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Phase != debridTypes.PhaseFailed || obs.Nudged {
		t.Fatalf("spent: phase %s nudged %v, want failed not nudged", obs.Phase, obs.Nudged)
	}
	if status, restarts := counts.status.Load(), counts.restarts.Load(); status != 2 || restarts != 1 {
		t.Fatalf("spent: %d status, %d restarts; want 2 and 1", status, restarts)
	}
}

func TestPollKeepsTheMissingMagnetError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"magnets":[]}}`)
	}))
	defer server.Close()
	_, _, err := testAllDebrid(server.URL).Poll(t.Context(), &debridTypes.Torrent{ID: "42"}, true)
	if !errors.Is(err, customerror.ErrTorrentNotFound) {
		t.Fatalf("error = %v, want ErrTorrentNotFound", err)
	}
}
