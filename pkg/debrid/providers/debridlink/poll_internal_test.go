package debridlink

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestPollMakesOneRequestPerRawState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  int
		errorID int
		phase   types.Phase
		cached  types.Verdict
	}{
		{"100 done", 100, 0, types.PhaseDownloaded, types.CachedUnknown},
		{"downloading", 3, 0, types.PhaseDownloading, types.CachedNo},
		{"error id", 6, 3, types.PhaseStalled, types.CachedUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				fmt.Fprintf(
					w,
					`{"success":true,"value":[{"id":"torrent","hashString":"hash","status":%d,"errorId":%d,"files":[{"id":"file","name":"movie.mkv","size":1000,"downloadUrl":"https://files.example/movie"}]}]}`,
					tc.status,
					tc.errorID,
				)
			}))
			defer server.Close()
			provider, err := New(config.Debrid{Name: "debridlink", APIKey: "token"}, nil, mkvOnly())
			if err != nil {
				t.Fatal(err)
			}
			provider.Host = server.URL
			got, obs, err := provider.Poll(
				t.Context(),
				&types.Torrent{ID: "torrent", Files: map[string]types.File{}},
				true,
			)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Phase != tc.phase || obs.Cached != tc.cached || obs.ID != "torrent" ||
				obs.Provider != "debridlink" {
				t.Fatalf("observation = %+v, want phase %s cached %d", obs, tc.phase, tc.cached)
			}
			if len(got.Files) != 1 {
				t.Fatalf("files = %d, want 1", len(got.Files))
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("%d requests, want 1", got)
			}
		})
	}
}
