package premiumize

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func pollProvider(t *testing.T, state string, link string, requests *atomic.Int32) *Premiumize {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/transfer/list", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprintf(
			w,
			`{"status":"success","transfers":[{"id":"t1","name":"Release","status":%q,"progress":1,"folder_id":"f1","file_id":null}]}`,
			state,
		)
	})
	mux.HandleFunc("GET /api/folder/list", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(
			w,
			`{"status":"success","folder_id":"f1","content":[{"id":"file-1","name":"movie.mkv","type":"file","size":1024,"link":%q}]}`,
			link,
		)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &Premiumize{
		Host:               server.URL,
		client:             request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		config:             config.Debrid{Name: "premiumize"},
		logger:             zerolog.Nop(),
		options:            types.ProviderOptions{ValidateFile: func(string, int64) error { return nil }},
		pollInterval:       time.Hour,
		maxFinalizingPolls: 5,
	}
}

func TestPollFinalizingReturnsAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		pm := pollProvider(t, "finished", "", &requests)
		start := time.Now()
		_, obs, err := pm.Poll(t.Context(), &types.Torrent{ID: "t1"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Phase != types.PhaseFinalizing {
			t.Fatalf("phase = %s, want finalizing", obs.Phase)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("%d list requests, want 1", got)
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("waited %s, want no wait", waited)
		}
	})
}

func TestPollMakesOneListPerRawState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state  string
		phase  types.Phase
		cached types.Verdict
	}{
		{"finished", types.PhaseDownloaded, types.CachedUnknown},
		{"queued", types.PhaseRemoteQueued, types.CachedNo},
		{"running", types.PhaseDownloading, types.CachedNo},
		{"waiting", types.PhaseRemoteQueued, types.CachedUnknown},
		{"error", types.PhaseFailed, types.CachedUnknown},
		{"future_state", types.PhaseUnknown, types.CachedUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			pm := pollProvider(t, tc.state, "https://example.test/movie.mkv", &requests)
			_, obs, err := pm.Poll(t.Context(), &types.Torrent{ID: "t1"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Phase != tc.phase || obs.Cached != tc.cached || obs.ID != "t1" || obs.Provider != "premiumize" {
				t.Fatalf("observation = %+v, want phase %s cached %d", obs, tc.phase, tc.cached)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("%d list requests, want 1", got)
			}
		})
	}
}
