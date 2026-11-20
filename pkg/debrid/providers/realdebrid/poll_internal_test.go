package realdebrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	probe := &types.Torrent{ID: "id", Name: "n", Added: time.Now().Add(-time.Hour)}
	_, _, err := testRealDebrid(server.URL).Poll(t.Context(), probe, true)
	requireUnconfirmed(t, err)
}

type listingReply func(w http.ResponseWriter, r *http.Request)

func listingBody(total, body string) listingReply {
	return func(w http.ResponseWriter, _ *http.Request) {
		if total != "" {
			w.Header().Set("X-Total-Count", total)
		}
		_, _ = fmt.Fprint(w, body)
	}
}

func listingStatus(status int) listingReply {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}
}

func distinctRows(n int) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":"r%d"}`, i)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

func serveInfo404(t *testing.T, reply listingReply) (string, *atomic.Int32) {
	t.Helper()
	var listings atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/torrents/info/"):
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/torrents":
			listings.Add(1)
			got, want := r.URL.Query(), url.Values{"offset": {"0"}, "limit": {"5000"}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("listing query = %v, want %v", got, want)
			}
			reply(w, r)
		case r.URL.Path == "/other":
			listingBody("0", "[]")(w, r)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, &listings
}

func settledProbe() *types.Torrent {
	return &types.Torrent{ID: "id", Name: "n", Added: time.Now().Add(-time.Hour)}
}

func requireNotFound(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(errors.Unwrap(err), customerror.ErrTorrentNotFound) {
		t.Fatalf("error = %v, want ErrTorrentNotFound as the sole cause", err)
	}
}

func requireUnconfirmed(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(errors.Unwrap(err), customerror.ErrProviderStatusTimeout) {
		t.Fatalf("error = %v, want ErrProviderStatusTimeout as the sole cause", err)
	}
	if errors.Is(err, customerror.ErrTorrentNotFound) {
		t.Fatalf("error = %v is also not found", err)
	}
	if !customerror.IsRetriableError(err) {
		t.Fatalf("error = %v is not retriable", err)
	}
}

func TestAnInfo404IsNotFoundOnlyWhenTheCompleteListingLacksIt(t *testing.T) {
	t.Parallel()
	redirect := func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/other", http.StatusFound) }
	twoHeaders := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("X-Total-Count", "1")
		w.Header().Add("X-Total-Count", "1")
		_, _ = fmt.Fprint(w, `[{"id":"x"}]`)
	}
	tests := []struct {
		name  string
		reply listingReply
		gone  bool
	}{
		{"listed", listingBody("1", `[{"id":"id","status":"downloading"}]`), false},
		{"listed in conversion", listingBody("1", `[{"id":"id","status":"magnet_conversion"}]`), false},
		{"absent complete", listingBody("1", `[{"id":"x"}]`), true},
		{"empty account", listingBody("0", `[]`), true},
		{"case differing row", listingBody("1", `[{"id":"ID"}]`), true},
		{"no header", listingBody("", `[{"id":"x"}]`), false},
		{"non canonical total", listingBody("02", `[{"id":"x"},{"id":"y"}]`), false},
		{"total above rows", listingBody("2", `[{"id":"x"}]`), false},
		{"null body", listingBody("0", `null`), false},
		{"empty id", listingBody("1", `[{"id":""}]`), false},
		{"repeated id", listingBody("2", `[{"id":"x"},{"id":"x"}]`), false},
		{"full page", listingBody("5000", distinctRows(5000)), false},
		{"no content", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Total-Count", "0")
			w.WriteHeader(http.StatusNoContent)
		}, false},
		{"two total headers", twoHeaders, false},
		{"redirected", redirect, false},
		{"unauthorized", listingStatus(http.StatusUnauthorized), false},
		{"unavailable", listingStatus(http.StatusServiceUnavailable), false},
		{"malformed", listingBody("1", `[{`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host, listings := serveInfo404(t, tc.reply)
			rd := testRealDebrid(host)
			rd.config.Limit = 5
			_, _, err := rd.Poll(t.Context(), settledProbe(), false)
			if tc.gone {
				requireNotFound(t, err)
			} else {
				requireUnconfirmed(t, err)
			}
			if got := listings.Load(); got != 1 {
				t.Fatalf("listing requests = %d, want 1", got)
			}
		})
	}
}

func TestAnInfo404WithTheListingReadCancelledIsUnconfirmed(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/torrents/info/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		close(arrived)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := testRealDebrid(server.URL).Poll(ctx, settledProbe(), false)
		result <- err
	}()
	select {
	case <-arrived:
		cancel()
		requireUnconfirmed(t, <-result)
	case err := <-result:
		t.Fatalf("Poll returned before any listing: %v", err)
	}
}

func TestAnUnsettledInfo404ListsNothing(t *testing.T) {
	t.Parallel()
	for name, added := range map[string]time.Time{"young": time.Now().Add(-30 * time.Second), "unknown": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host, listings := serveInfo404(t, listingBody("0", `[]`))
			probe := &types.Torrent{ID: "id", Name: "n", Added: added}
			_, _, err := testRealDebrid(host).Poll(t.Context(), probe, false)
			requireUnconfirmed(t, err)
			if got := listings.Load(); got != 0 {
				t.Fatalf("listing requests = %d, want 0", got)
			}
		})
	}
}
