package reacquire

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

type recordedProgress struct {
	statuses []Status
}

func (progress *recordedProgress) Update(status Status, _ func(*Job)) error {
	progress.statuses = append(progress.statuses, status)
	return nil
}

func (progress *recordedProgress) UpdateDurable(status Status, mutate func(*Job)) error {
	return progress.Update(status, mutate)
}

func TestReacquireHandlerFailsExactDownloadAndWaitsForArr(t *testing.T) {
	t.Parallel()
	var historyCalls atomic.Int64
	var failed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/moviefile/42":
			_, _ = fmt.Fprint(w, `{"id":42,"movieId":9,"path":"/library/movie.mkv"}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/api/v3/moviefile/42":
			w.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/config/downloadclient":
			_, _ = fmt.Fprint(w, `{"enableCompletedDownloadHandling":true,"autoRedownloadFailed":true}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			call := historyCalls.Add(1)
			if call == 1 {
				_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":0,"records":[]}`)
			} else {
				_, _ = fmt.Fprint(
					w,
					`{"page":1,"totalRecords":1,"records":[{"id":7,"downloadId":"download-1","eventType":"grabbed"}]}`,
				)
			}
		case request.Method == http.MethodPost && request.URL.Path == "/api/v3/history/failed/7":
			failed.Store(true)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	registry := newTestArrStorage()
	instance := arr.Arr{Name: "movies", Host: server.URL, Token: "secret", Type: arr.Radarr}
	registry.AddOrUpdate(instance)
	handler := NewHandler(registry, nil)
	progress := &recordedProgress{}
	job := Job{
		ArrName:    "movies",
		ArrType:    arr.Radarr,
		EntryID:    "entry-1",
		FileID:     "file-1",
		DownloadID: "download-1",
		Strategy:   StrategyHistoryFailed,
		Bindings: []Binding{{
			ArrName:                "movies",
			ArrType:                arr.Radarr,
			ArrInstanceFingerprint: instance.Fingerprint(),
			EntryID:                "entry-1",
			EntryFileID:            "file-1",
			DownloadID:             "download-1",
			ArrFileID:              42,
			LibraryPath:            "/library/movie.mkv",
			MovieID:                9,
			Confidence:             ConfidenceExactPath,
		}},
	}
	if err := handler.Reacquire(t.Context(), job, progress); err != nil {
		t.Fatal(err)
	}
	if !failed.Load() {
		t.Fatal("history record was not marked failed")
	}
	if got := progress.statuses[len(progress.statuses)-1]; got != StatusWaitingForGrab {
		t.Fatalf("last status = %q, want %q", got, StatusWaitingForGrab)
	}
}

// The arr keeps a torrent's download ID uppercased; a job holding the lowercase infohash still finds and fails its grab.
func TestReacquireHandlerFailsAnUppercaseTorrentGrab(t *testing.T) {
	t.Parallel()
	const infohash = "0123456789abcdef0123456789abcdef01234567"
	stored := strings.ToUpper(infohash)
	var failed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/moviefile/42":
			_, _ = fmt.Fprint(w, `{"id":42,"movieId":9,"path":"/library/movie.mkv"}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/api/v3/moviefile/42":
			w.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/config/downloadclient":
			_, _ = fmt.Fprint(w, `{"enableCompletedDownloadHandling":true,"autoRedownloadFailed":true}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			if request.URL.Query().Get("downloadId") != stored {
				_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":0,"records":[]}`)
				return
			}
			_, _ = fmt.Fprintf(
				w,
				`{"page":1,"totalRecords":1,"records":[{"id":7,"downloadId":%q,"eventType":"grabbed"}]}`,
				stored,
			)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v3/history/failed/7":
			failed.Store(true)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	registry := newTestArrStorage()
	instance := arr.Arr{Name: "movies", Host: server.URL, Token: "secret", Type: arr.Radarr}
	registry.AddOrUpdate(instance)
	progress := &recordedProgress{}
	job := Job{
		ArrName:    "movies",
		ArrType:    arr.Radarr,
		EntryID:    "entry-1",
		FileID:     "file-1",
		DownloadID: infohash,
		Strategy:   StrategyHistoryFailed,
		Bindings: []Binding{{
			ArrName:                "movies",
			ArrType:                arr.Radarr,
			ArrInstanceFingerprint: instance.Fingerprint(),
			EntryID:                "entry-1",
			EntryFileID:            "file-1",
			DownloadID:             infohash,
			ArrFileID:              42,
			LibraryPath:            "/library/movie.mkv",
			MovieID:                9,
			Confidence:             ConfidenceExactPath,
		}},
	}
	if err := NewHandler(registry, nil).Reacquire(t.Context(), job, progress); err != nil {
		t.Fatal(err)
	}
	if !failed.Load() {
		t.Fatal("uppercase grab was not marked failed")
	}
	if got := progress.statuses[len(progress.statuses)-1]; got != StatusWaitingForGrab {
		t.Fatalf("last status = %q, want %q", got, StatusWaitingForGrab)
	}
}

func TestReacquireHandlerRefusesStaleArrFileIdentity(t *testing.T) {
	t.Parallel()
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/moviefile/42":
			_, _ = fmt.Fprint(w, `{"id":42,"movieId":9,"path":"/library/other.mkv"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/config/downloadclient":
			_, _ = fmt.Fprint(w, `{"enableCompletedDownloadHandling":true}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/api/v3/moviefile/42":
			deleted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	registry := newTestArrStorage()
	instance := arr.Arr{Name: "movies", Host: server.URL, Token: "secret", Type: arr.Radarr}
	registry.AddOrUpdate(instance)
	job := Job{
		ArrName:  "movies",
		ArrType:  arr.Radarr,
		EntryID:  "entry-1",
		FileID:   "file-1",
		Strategy: StrategyCommandSearch,
		Bindings: []Binding{{
			ArrName:                "movies",
			ArrType:                arr.Radarr,
			ArrInstanceFingerprint: instance.Fingerprint(),
			EntryID:                "entry-1",
			EntryFileID:            "file-1",
			ArrFileID:              42,
			LibraryPath:            "/library/movie.mkv",
			MovieID:                9,
			Confidence:             ConfidenceExactPath,
		}},
	}
	err := NewHandler(registry, nil).Reacquire(t.Context(), job, &recordedProgress{})
	if err == nil {
		t.Fatal("expected stale Arr identity to be rejected")
	}
	if deleted.Load() {
		t.Fatal("stale Arr file was deleted")
	}
}

func TestReacquireHandlerRefusesChangedArrInstance(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.NotFound(w, request)
	}))
	defer server.Close()

	registry := newTestArrStorage()
	registry.AddOrUpdate(arr.Arr{Name: "movies", Host: server.URL, Token: "secret", Type: arr.Radarr})
	job := Job{
		ArrName:  "movies",
		ArrType:  arr.Radarr,
		EntryID:  "entry-1",
		FileID:   "file-1",
		Strategy: StrategyCommandSearch,
		Bindings: []Binding{{
			ArrName:                "movies",
			ArrType:                arr.Radarr,
			ArrInstanceFingerprint: "v1:old-instance",
			EntryID:                "entry-1",
			EntryFileID:            "file-1",
			ArrFileID:              42,
			LibraryPath:            "/library/movie.mkv",
			MovieID:                9,
			Confidence:             ConfidenceExactPath,
		}},
	}
	err := NewHandler(registry, nil).Reacquire(t.Context(), job, &recordedProgress{})
	if err == nil {
		t.Fatal("expected changed Arr instance to be rejected")
	}
	if requests.Load() != 0 {
		t.Fatalf("changed Arr instance received %d requests", requests.Load())
	}
}

func TestAutoRedownloadsFailureHonorsInteractiveSourceConfig(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		config arr.DownloadClientConfig
		source string
		want   bool
	}{
		{name: "master disabled", config: arr.DownloadClientConfig{AutoRedownloadFailedFromInteractiveSearch: true}, source: "InteractiveSearch"},
		{name: "automatic source", config: arr.DownloadClientConfig{AutoRedownloadFailed: true}, source: "Search", want: true},
		{name: "missing source", config: arr.DownloadClientConfig{AutoRedownloadFailed: true}, want: true},
		{name: "interactive disabled", config: arr.DownloadClientConfig{AutoRedownloadFailed: true}, source: "InteractiveSearch"},
		{name: "interactive enabled", config: arr.DownloadClientConfig{AutoRedownloadFailed: true, AutoRedownloadFailedFromInteractiveSearch: true}, source: "InteractiveSearch", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			failure := exactDownloadFailure{
				grabRecord: arr.HistoryRecord{Data: map[string]string{"releaseSource": test.source}},
			}
			if got := test.config.RedownloadsFailed(failure.grabRecord); got != test.want {
				t.Fatalf("RedownloadsFailed() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSearchBindingsReconcilesPersistedCommandWithoutRedispatch(t *testing.T) {
	t.Parallel()
	dispatched := atomic.Int64{}
	queued := time.Now().UTC().Add(-time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			_, _ = fmt.Fprintf(
				w,
				`[{"id":71,"name":"MoviesSearch","queued":%q,"body":{"name":"MoviesSearch","movieIds":[9]}}]`,
				queued.Format(time.RFC3339Nano),
			)
		case http.MethodPost:
			dispatched.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	mutation := Mutation{
		Key:              mutationKey(MutationMovieSearch, "9"),
		Kind:             MutationMovieSearch,
		State:            MutationIntent,
		CommandName:      "MoviesSearch",
		MovieIDs:         []int{9},
		IntentAt:         queued.Add(-time.Second),
		LastDispatchedAt: queued,
		Attempts:         1,
	}
	job := Job{Mutations: []Mutation{mutation}}
	status, err := newTestHandler(server.URL).searchBindings(
		t.Context(),
		testInstance(server.URL),
		&job,
		[]Binding{{ArrType: arr.Radarr, MovieID: 9}},
		&recordedProgress{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusWaitingForGrab {
		t.Fatalf("status = %q", status)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("search was redispatched %d times", dispatched.Load())
	}
	confirmed, ok := mutationForKey(job, mutation.Key)
	if !ok || confirmed.State != MutationConfirmed || confirmed.ReceiptID != 71 {
		t.Fatalf("confirmed mutation = %#v, found = %v", confirmed, ok)
	}
}

func TestSearchBindingsWaitsForCommandVisibilityBeforeRedispatch(t *testing.T) {
	t.Parallel()
	dispatched := atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			dispatched.Add(1)
		}
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	now := time.Now().UTC()
	mutation := Mutation{
		Key:              mutationKey(MutationMovieSearch, "9"),
		Kind:             MutationMovieSearch,
		State:            MutationIntent,
		CommandName:      "MoviesSearch",
		MovieIDs:         []int{9},
		IntentAt:         now.Add(-time.Second),
		LastDispatchedAt: now,
		Attempts:         1,
	}
	job := Job{Mutations: []Mutation{mutation}}
	_, err := newTestHandler(server.URL).searchBindings(
		t.Context(),
		testInstance(server.URL),
		&job,
		[]Binding{{ArrType: arr.Radarr, MovieID: 9}},
		&recordedProgress{},
	)
	if !errors.Is(err, arr.ErrMutationOutcomeUnknown) {
		t.Fatalf("error = %v, want unknown mutation outcome", err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("search was redispatched %d times", dispatched.Load())
	}
}

func TestPreexistingFailedHistoryUsesExplicitSearch(t *testing.T) {
	t.Parallel()
	searches := atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v3/command" {
			http.NotFound(w, request)
			return
		}
		searches.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":72,"name":"MoviesSearch","status":"queued"}`)
	}))
	defer server.Close()

	job := Job{}
	status, err := newTestHandler(server.URL).failHistory(
		t.Context(),
		testInstance(server.URL),
		&job,
		[]Binding{{ArrType: arr.Radarr, MovieID: 9}},
		exactDownloadFailure{found: true, alreadyFailed: true, downloadID: "download-1", failedID: 18},
		arr.DownloadClientConfig{AutoRedownloadFailed: true},
		&recordedProgress{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusWaitingForGrab || searches.Load() != 1 {
		t.Fatalf("status = %q, searches = %d", status, searches.Load())
	}
}

// A history_failed job blocklists each grab of its download ID once: a re-grab
// of the same release after its search is blocklisted and searched for again
// when the Arr does not redownload by itself; a resumed job does neither twice.
func TestResumedHistoryFailedBlocklistsEachGrabOnce(t *testing.T) {
	t.Parallel()
	queued := time.Now().UTC().Add(-time.Minute)
	search := Mutation{
		Key:              mutationKey(MutationMovieSearch, "9"),
		Kind:             MutationMovieSearch,
		State:            MutationIntent,
		CommandName:      "MoviesSearch",
		MovieIDs:         []int{9},
		IntentAt:         queued.Add(-time.Second),
		LastDispatchedAt: queued,
		Attempts:         1,
	}
	searched := search
	searched.State, searched.ReceiptID, searched.ConfirmedAt = MutationConfirmed, 71, queued
	blocklist := Mutation{
		Key:              mutationKey(MutationHistoryFailed, "download-1"),
		Kind:             MutationHistoryFailed,
		State:            MutationConfirmed,
		DownloadID:       "download-1",
		HistoryID:        12,
		IntentAt:         queued.Add(-time.Minute),
		LastDispatchedAt: queued.Add(-time.Minute),
		Attempts:         1,
		ReceiptID:        13,
		ConfirmedAt:      queued.Add(-time.Minute),
	}
	regrabBlocklist := blocklist
	regrabBlocklist.Key, regrabBlocklist.HistoryID, regrabBlocklist.ReceiptID =
		mutationKey(MutationHistoryFailed, "download-1", "17"), 17, 18
	regrabAttempted := regrabBlocklist
	regrabAttempted.State, regrabAttempted.ReceiptID, regrabAttempted.ConfirmedAt = MutationIntent, 0, time.Time{}
	regrabSearch := searched
	regrabSearch.Key = mutationKey(MutationMovieSearch, "9", "round-1")
	regrabSearchAttempted := search
	regrabSearchAttempted.Key = regrabSearch.Key
	blocklistSelfFailed := blocklist
	blocklistSelfFailed.Attempts = 0
	legacySelfFailed := blocklistSelfFailed
	legacySelfFailed.HistoryID = 0
	thirdGrab := exactDownloadFailure{
		found: true, grabFound: true, downloadID: "download-1", historyID: 30,
		grabRecord: arr.HistoryRecord{ID: 30},
	}
	searchUnsent := search
	searchUnsent.Attempts, searchUnsent.LastDispatchedAt = 0, time.Time{}
	blocklistAttempted := blocklist
	blocklistAttempted.State, blocklistAttempted.ReceiptID, blocklistAttempted.ConfirmedAt = MutationIntent, 0, time.Time{}
	regrabbed := exactDownloadFailure{
		found: true, grabFound: true, downloadID: "download-1", historyID: 17,
		grabRecord: arr.HistoryRecord{ID: 17},
	}
	regrabFailed := exactDownloadFailure{
		found: true, grabFound: true, alreadyFailed: true, downloadID: "download-1", historyID: 17, failedID: 18,
		grabRecord: arr.HistoryRecord{ID: 17},
	}
	firstGrab := exactDownloadFailure{
		found: true, grabFound: true, downloadID: "download-1", historyID: 12,
		grabRecord: arr.HistoryRecord{ID: 12},
	}
	for name, tc := range map[string]struct {
		journal        []Mutation
		failure        exactDownloadFailure
		autoRedownload bool
		wantBlocklists int64
		wantSearches   int64
		wantConfirmed  string
		wantReceipt    int
		keepsIntent    bool
	}{
		"searched then re-grabbed": {
			journal: []Mutation{search}, failure: regrabbed, autoRedownload: true, wantBlocklists: 1, keepsIntent: true,
		},
		"re-grabbed after a confirmed blocklist": {
			journal: []Mutation{blocklist, searched}, failure: regrabbed, autoRedownload: true, wantBlocklists: 1,
		},
		"re-grabbed after a confirmed blocklist, no auto redownload": {
			journal: []Mutation{blocklist, searched}, failure: regrabbed, wantBlocklists: 1, wantSearches: 1,
		},
		"resumed after the re-grab round": {
			journal: []Mutation{blocklist, searched, regrabBlocklist, regrabSearch}, failure: regrabFailed,
		},
		"resumed after a re-grab blocklist the Arr recorded": {
			journal: []Mutation{blocklist, searched, regrabAttempted}, failure: regrabFailed, wantSearches: 1,
			wantConfirmed: regrabBlocklist.Key, wantReceipt: 18,
		},
		"re-grabbed while the earlier search is unsettled": {
			journal: []Mutation{blocklist, search}, failure: regrabbed, wantBlocklists: 1, wantSearches: 1,
			wantConfirmed: search.Key, wantReceipt: 71,
		},
		"resumed after a re-grab blocklist, first failure by the Arr": {
			journal: []Mutation{blocklistSelfFailed, searched, regrabAttempted}, failure: regrabFailed,
			autoRedownload: true, wantConfirmed: regrabBlocklist.Key, wantReceipt: 18,
		},
		"grabbed a third time while the re-grab search is unsettled": {
			journal: []Mutation{blocklist, searched, regrabBlocklist, regrabSearchAttempted},
			failure: thirdGrab, wantBlocklists: 1, wantSearches: 1,
			wantConfirmed: regrabSearch.Key, wantReceipt: 71,
		},
		"re-grabbed after an earlier search never sent": {
			journal: []Mutation{blocklist, searchUnsent}, failure: regrabbed, wantBlocklists: 1, wantSearches: 2,
			wantConfirmed: search.Key, wantReceipt: 72,
		},
		"grabbed a third time while the re-grab blocklist is unsettled": {
			journal: []Mutation{blocklist, searched, regrabAttempted}, failure: thirdGrab, autoRedownload: true,
			wantBlocklists: 2, wantConfirmed: regrabBlocklist.Key, wantReceipt: 17,
		},
		"re-grabbed while the first blocklist is unsettled": {
			journal: []Mutation{blocklistAttempted}, failure: regrabbed, autoRedownload: true, wantBlocklists: 1,
			wantConfirmed: blocklist.Key, wantReceipt: 12,
		},
		"searched before the first blocklist, no auto redownload": {
			journal: []Mutation{searched}, failure: firstGrab, wantBlocklists: 1, wantSearches: 1,
		},
		"re-grab the Arr failed itself, no auto redownload": {
			journal: []Mutation{blocklist, searched}, failure: regrabFailed, wantSearches: 1,
			wantConfirmed: regrabBlocklist.Key, wantReceipt: 18,
		},
		"re-grab the Arr failed itself": {
			journal: []Mutation{blocklist, searched}, failure: regrabFailed, autoRedownload: true,
			wantConfirmed: regrabBlocklist.Key, wantReceipt: 18,
		},
		"resumed on an Arr-recorded first failure an older binary journaled": {
			journal: []Mutation{legacySelfFailed, searched}, failure: exactDownloadFailure{
				found: true, grabFound: true, alreadyFailed: true, downloadID: "download-1", historyID: 12,
				failedID: 13, grabRecord: arr.HistoryRecord{ID: 12},
			},
		},
		"already blocklisted": {
			journal: []Mutation{blocklist, searched}, failure: exactDownloadFailure{
				found: true, grabFound: true, alreadyFailed: true, downloadID: "download-1", historyID: 12,
				failedID: 13, grabRecord: arr.HistoryRecord{ID: 12},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server, blocklisted, searches := newSearchedMovieArr(t, queued)
			job := Job{Mutations: tc.journal}
			status, err := newTestHandler(server.URL).failHistory(
				t.Context(),
				testInstance(server.URL),
				&job,
				[]Binding{{ArrType: arr.Radarr, MovieID: 9}},
				tc.failure,
				arr.DownloadClientConfig{AutoRedownloadFailed: tc.autoRedownload},
				&recordedProgress{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if status != StatusWaitingForGrab || blocklisted.Load() != tc.wantBlocklists ||
				searches.Load() != tc.wantSearches {
				t.Fatalf("status = %q, blocklisted = %d, searched = %d, want %d blocklists, %d searches",
					status, blocklisted.Load(), searches.Load(), tc.wantBlocklists, tc.wantSearches)
			}
			if confirmed, _ := mutationForKey(job, tc.wantConfirmed); tc.wantConfirmed != "" &&
				(confirmed.State != MutationConfirmed || confirmed.ReceiptID != tc.wantReceipt) {
				t.Fatalf("%s = %+v, want confirmed by %d", tc.wantConfirmed, confirmed, tc.wantReceipt)
			}
			if deadline := job.reconciliationDeadline(); !tc.keepsIntent && !deadline.IsZero() {
				t.Fatalf("journal %+v keeps an intent, deadline %v", job.Mutations, deadline)
			}
		})
	}
}

// A grab the Arr already failed keeps its grab ID, so a later re-grab is told apart from it.
func TestAlreadyFailedGrabKeepsItsHistoryID(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":2,"records":[`+
			`{"id":18,"downloadId":"download-1","eventType":"downloadFailed"},`+
			`{"id":17,"downloadId":"download-1","eventType":"grabbed"}]}`)
	}))
	t.Cleanup(server.Close)
	failure, err := newTestHandler(server.URL).prepareExactDownloadFailure(
		t.Context(), testInstance(server.URL), "download-1",
	)
	if err != nil || !failure.alreadyFailed || failure.historyID != 17 || failure.failedID != 18 {
		t.Fatalf("failure = %+v, err = %v, want already failed grab 17 by record 18", failure, err)
	}
}

func TestDownloadGrabbedAgainAfterEveryRoundFailsTheJob(t *testing.T) {
	t.Parallel()
	queued := time.Now().UTC().Add(-time.Minute)
	journal := []Mutation{}
	for _, grab := range []int{12, 17, 30, 40} {
		key := mutationKey(MutationHistoryFailed, "download-1", strconv.Itoa(grab))
		if grab == 12 {
			key = mutationKey(MutationHistoryFailed, "download-1")
		}
		journal = append(journal, Mutation{
			Key: key, Kind: MutationHistoryFailed, State: MutationConfirmed, DownloadID: "download-1",
			HistoryID: grab, IntentAt: queued, LastDispatchedAt: queued, Attempts: 1, ReceiptID: grab,
			ConfirmedAt: queued,
		})
	}
	server, blocklisted, _ := newSearchedMovieArr(t, queued)
	job := Job{Mutations: journal}
	err := newTestHandler(server.URL).executeExactDownloadFailure(
		t.Context(), testInstance(server.URL), &job, exactDownloadFailure{
			found: true, grabFound: true, downloadID: "download-1", historyID: 50,
			grabRecord: arr.HistoryRecord{ID: 50},
		}, &recordedProgress{},
	)
	if err == nil || errors.Is(err, arr.ErrMutationOutcomeUnknown) ||
		blocklisted.Load() != 0 || len(job.Mutations) != 4 {
		t.Fatalf("err = %v, blocklisted = %d, journal = %d, want a plain error and nothing sent", err,
			blocklisted.Load(), len(job.Mutations))
	}
}

// A job that gives up on a download grabbed once too often leaves the library file alone.
func TestGivingUpOnARegrabDeletesNoFile(t *testing.T) {
	t.Parallel()
	queued := time.Now().UTC().Add(-time.Minute)
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/moviefile/42":
			_, _ = fmt.Fprint(w, `{"id":42,"movieId":9,"path":"/library/movie.mkv"}`)
		case request.Method == http.MethodDelete && request.URL.Path == "/api/v3/moviefile/42":
			deleted.Store(true)
			w.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/config/downloadclient":
			_, _ = fmt.Fprint(w, `{"enableCompletedDownloadHandling":true,"autoRedownloadFailed":true}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			_, _ = fmt.Fprint(w,
				`{"page":1,"totalRecords":1,"records":[{"id":50,"downloadId":"download-1","eventType":"grabbed"}]}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	registry := newTestArrStorage()
	instance := arr.Arr{Name: "movies", Host: server.URL, Token: "secret", Type: arr.Radarr}
	registry.AddOrUpdate(instance)
	job := Job{
		ArrName: "movies", ArrType: arr.Radarr, EntryID: "entry-1", FileID: "file-1", DownloadID: "download-1",
		Strategy: StrategyHistoryFailed,
		Bindings: []Binding{{
			ArrName: "movies", ArrType: arr.Radarr, ArrInstanceFingerprint: instance.Fingerprint(),
			EntryID: "entry-1", EntryFileID: "file-1", DownloadID: "download-1", ArrFileID: 42,
			LibraryPath: "/library/movie.mkv", MovieID: 9, Confidence: ConfidenceExactPath,
		}},
	}
	for _, grab := range []int{12, 17, 30, 40} {
		key := mutationKey(MutationHistoryFailed, "download-1", strconv.Itoa(grab))
		if grab == 12 {
			key = mutationKey(MutationHistoryFailed, "download-1")
		}
		job.Mutations = append(job.Mutations, Mutation{
			Key: key, Kind: MutationHistoryFailed, State: MutationConfirmed, DownloadID: "download-1",
			HistoryID: grab, IntentAt: queued, LastDispatchedAt: queued, Attempts: 1, ReceiptID: grab,
			ConfirmedAt: queued,
		})
	}
	if err := NewHandler(registry, nil).Reacquire(t.Context(), job, &recordedProgress{}); err == nil {
		t.Fatal("Reacquire succeeded, want it to give up")
	}
	if deleted.Load() {
		t.Fatal("library file was deleted before giving up")
	}
}

// An earlier blocklist sent again fails every grab of the download, so the current one is not sent twice.
func TestSettledEarlierBlocklistCoversTheCurrentGrab(t *testing.T) {
	t.Parallel()
	queued := time.Now().UTC().Add(-time.Minute)
	var blocklisted atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/v3/history/failed/"):
			blocklisted.Add(1)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			if blocklisted.Load() == 0 {
				_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":0,"records":[]}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":1,"records":[`+
				`{"id":31,"downloadId":"download-1","eventType":"downloadFailed"}]}`)
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	first := Mutation{
		Key: mutationKey(MutationHistoryFailed, "download-1"), Kind: MutationHistoryFailed,
		State: MutationConfirmed, DownloadID: "download-1", HistoryID: 12, IntentAt: queued,
		LastDispatchedAt: queued, Attempts: 1, ReceiptID: 13, ConfirmedAt: queued,
	}
	unsettled := Mutation{
		Key: mutationKey(MutationHistoryFailed, "download-1", "17"), Kind: MutationHistoryFailed,
		State: MutationIntent, DownloadID: "download-1", HistoryID: 17, IntentAt: queued,
		LastDispatchedAt: queued.Add(-time.Minute), Attempts: 1,
	}
	job := Job{Mutations: []Mutation{first, unsettled}}
	err := newTestHandler(server.URL).executeExactDownloadFailure(
		t.Context(), testInstance(server.URL), &job, exactDownloadFailure{
			found: true, grabFound: true, downloadID: "download-1", historyID: 30,
			grabRecord: arr.HistoryRecord{ID: 30},
		}, &recordedProgress{},
	)
	if err != nil || blocklisted.Load() != 1 {
		t.Fatalf("err = %v, blocklisted = %d, want one blocklist", err, blocklisted.Load())
	}
	current, _ := mutationForKey(job, mutationKey(MutationHistoryFailed, "download-1", "30"))
	if current.State != MutationConfirmed || current.ReceiptID != 31 || current.Attempts != 0 {
		t.Fatalf("current blocklist = %+v, want confirmed by record 31 without a send", current)
	}
}

func TestRegrabSearchSettlesOnlySearches(t *testing.T) {
	t.Parallel()
	queued := time.Now().UTC().Add(-time.Minute)
	unsettled := Mutation{
		Key: mutationKey(MutationHistoryFailed, "download-1", "30"), Kind: MutationHistoryFailed,
		State: MutationIntent, DownloadID: "download-1", HistoryID: 30,
		IntentAt: queued, LastDispatchedAt: queued, Attempts: 1,
	}
	server, blocklisted, searches := newSearchedMovieArr(t, queued)
	job := Job{Mutations: []Mutation{unsettled}}
	status, err := newTestHandler(server.URL).searchBindings(
		t.Context(), testInstance(server.URL), &job, []Binding{{ArrType: arr.Radarr, MovieID: 9}}, &recordedProgress{},
	)
	if err != nil || status != StatusWaitingForGrab || blocklisted.Load() != 0 || searches.Load() != 1 {
		t.Fatalf("status = %q, err = %v, blocklisted = %d, searched = %d", status, err, blocklisted.Load(),
			searches.Load())
	}
	if got, _ := mutationForKey(job, unsettled.Key); got.State != MutationIntent || got.Attempts != 1 {
		t.Fatalf("blocklist = %+v, want it left as it was", got)
	}
}

// newSearchedMovieArr serves an Arr whose command list already shows the
// MoviesSearch for movie 9, an empty history and a blocklist POST for any grab,
// counting blocklist and search POSTs.
func newSearchedMovieArr(t *testing.T, queued time.Time) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	blocklisted, searched := &atomic.Int64{}, &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/v3/history/failed/"):
			blocklisted.Add(1)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":0,"records":[]}`)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v3/command":
			searched.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":72,"name":"MoviesSearch","status":"queued"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/command":
			_, _ = fmt.Fprintf(
				w,
				`[{"id":71,"name":"MoviesSearch","queued":%q,"body":{"name":"MoviesSearch","movieIds":[9]}}]`,
				queued.Format(time.RFC3339Nano),
			)
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	return server, blocklisted, searched
}

func testInstance(host string) arr.Arr {
	return arr.Arr{Name: "movies", Host: host, Token: "secret", Type: arr.Radarr}
}

func newTestHandler(host string) *arrHandler {
	arrs := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	arrs.AddOrUpdate(testInstance(host))
	return &arrHandler{arrs: arrs}
}

func newTestArrStorage() *arr.Service {
	return arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
}
