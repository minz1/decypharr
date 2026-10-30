package reacquire

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

// fakeRadarrLibrary serves one Radarr movie file (42) that can be deleted and
// later replaced by an imported file (43).
type fakeRadarrLibrary struct {
	deleted, imported atomic.Bool
	searches          atomic.Int64
}

func (fake *fakeRadarrLibrary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/moviefile/42":
		if fake.deleted.Load() {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":42,"movieId":9,"path":"/library/movie.mkv"}`)
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/moviefile/42":
		fake.deleted.Store(true)
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/api/v3/config/downloadclient":
		fmt.Fprint(w, `{"enableCompletedDownloadHandling":true}`)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
		fake.searches.Add(1)
		fmt.Fprint(w, `{"id":123,"name":"MoviesSearch"}`)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie/9":
		if fake.imported.Load() {
			fmt.Fprint(w, `{"id":9,"movieFile":{"id":43,"movieId":9,"path":"/library/new.mkv"}}`)
		} else {
			fmt.Fprint(w, `{"id":9}`)
		}
	default:
		http.NotFound(w, r)
	}
}

// startTestService opens and starts a Service over directory.
func startTestService(t *testing.T, directory string, registry *arr.Service) *Service {
	t.Helper()
	service, err := NewService(ServiceOptions{Directory: directory, Arrs: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	return service
}

// runLibraryJobToWaiting checks a wrong path is refused without work, then
// runs the verified request until it waits for the replacement.
func runLibraryJobToWaiting(
	t *testing.T,
	service *Service,
	registry *arr.Service,
	fake *fakeRadarrLibrary,
	request LibraryRequest,
) *Job {
	t.Helper()
	wrong := request
	wrong.LibraryPath = "/library/wrong.mkv"
	if _, err := service.ReacquireLibraryFile(t.Context(), wrong); !errors.Is(err, ErrBindingUnsafe) {
		t.Fatalf("wrong path: %v", err)
	}
	if len(service.Jobs()) != 0 || fake.deleted.Load() {
		t.Fatal("unverified request caused work")
	}
	job, err := service.ReacquireLibraryFile(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !service.runJob(t.Context(), NewHandler(registry, nil), *job) {
		t.Fatal("job failed to run")
	}
	waiting, _ := service.Job(job.ID)
	if !waiting.Status.waiting() || len(waiting.Mutations) != 1 || waiting.Mutations[0].State != MutationConfirmed {
		t.Fatalf("job = %#v", waiting)
	}
	if !fake.deleted.Load() || fake.searches.Load() != 1 {
		t.Fatalf("deleted=%v searches=%d", fake.deleted.Load(), fake.searches.Load())
	}
	service.reconcileImportedJobs(t.Context())
	if waiting, _ = service.Job(job.ID); !waiting.Status.waiting() {
		t.Fatal("missing replacement completed job")
	}
	return job
}

func TestLibraryRecoveryUsesDurableJobsAndWaitsForReplacement(t *testing.T) {
	t.Parallel()
	fake := &fakeRadarrLibrary{}
	server := httptest.NewServer(fake)
	defer server.Close()
	registry := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	registry.AddOrUpdate(arr.Arr{Name: "movies", Type: arr.Radarr, Host: server.URL, Token: "token"})
	directory := t.TempDir()
	service := startTestService(t, directory, registry)
	request := LibraryRequest{ArrName: "movies", ArrFileID: 42, LibraryPath: "/library/movie.mkv", Cause: CauseRepair}
	job := runLibraryJobToWaiting(t, service, registry, fake, request)
	if closeErr := service.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	reopened := startTestService(t, directory, registry)
	duplicate, err := reopened.ReacquireLibraryFile(t.Context(), request)
	if err != nil || duplicate.ID != job.ID {
		t.Fatalf("duplicate after delete/restart = %v, %v", duplicate, err)
	}
	binding := job.Bindings[0]
	binding.EntryID, binding.EntryFileID, binding.DownloadID = "managed-entry", "managed-file", "download"
	binding.Confidence = ConfidenceExactPath
	// Indexing the original file cannot create a second mutation owner.
	if upsertBindingErr := reopened.UpsertBinding(binding); upsertBindingErr != nil {
		t.Fatal(upsertBindingErr)
	}
	duplicate, err = reopened.Reacquire(
		Request{EntryID: binding.EntryID, FileID: binding.EntryFileID, Cause: CauseStream},
	)
	if err != nil || duplicate.ID != job.ID {
		t.Fatalf("indexed duplicate = %v, %v", duplicate, err)
	}
	fake.imported.Store(true)
	reopened.reconcileImportedJobs(t.Context())
	ready, _ := reopened.Job(job.ID)
	if ready.Status != StatusReady || fake.searches.Load() != 1 {
		t.Fatalf("status=%s searches=%d", ready.Status, fake.searches.Load())
	}
}

// importedJobCase is one Arr library state reconcileImportedJobs must judge.
type importedJobCase struct {
	name            string
	kind            arr.Type
	movie           string
	files           string
	episodes        string
	ready           bool
	changedInstance bool
	unauthorized    bool
	sameDownload    bool
	history         string
}

// serveImportedJobCase answers the read-only library requests for tc and
// counts them; any mutation fails the test.
func serveImportedJobCase(t *testing.T, tc importedJobCase, requests *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Error("confirmation sent a mutation")
		}
		if tc.unauthorized || r.URL.Path == "/api/v3/history" && tc.history == "unauthorized" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		history := tc.history
		if history == "" {
			history = `{"page":1,"totalRecords":0,"records":[]}`
		}
		body := map[string]string{
			"/api/v3/movie/9":     tc.movie,
			"/api/v3/episodefile": tc.files,
			"/api/v3/episode":     tc.episodes,
			"/api/v3/history":     history,
		}
		response, ok := body[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/api/v3/history" && r.URL.Query().Get("downloadId") != "same-download" {
			t.Error("wrong download ID")
		}
		if r.URL.Path != "/api/v3/movie/9" && r.URL.Path != "/api/v3/history" && r.URL.Query().Get("seriesId") != "9" {
			t.Error("wrong series ID")
		}
		fmt.Fprint(w, response)
	}))
	t.Cleanup(server.Close)
	return server
}

// waitingImportJob builds a job, already past its waiting timeout, replacing
// file 42 (and 43 for Sonarr) of media 9.
func waitingImportJob(tc importedJobCase, instance arr.Arr, now time.Time) (Job, Binding) {
	binding := Binding{
		ArrName: instance.Name, ArrType: instance.Type, ArrInstanceFingerprint: instance.Fingerprint(),
		EntryID: "managed-entry", EntryFileID: "first-file", DownloadID: "same-download",
		ArrFileID: 42, LibraryPath: "/library/old.mkv", Confidence: ConfidenceExactPath,
	}
	if tc.changedInstance {
		binding.ArrInstanceFingerprint = "different-instance"
	}
	bindings := []Binding{binding}
	if tc.kind == arr.Radarr {
		bindings[0].MovieID = 9
	} else {
		bindings[0].SeriesID = 9
		bindings[0].EpisodeIDs = []int{101}
		second := bindings[0]
		second.EntryFileID, second.ArrFileID, second.EpisodeIDs = "second-file", 43, []int{102}
		bindings = append(bindings, second)
	}
	return Job{
		ID:         "waiting-job",
		ArrName:    instance.Name,
		ArrType:    instance.Type,
		EntryID:    binding.EntryID,
		FileID:     binding.EntryFileID,
		DownloadID: binding.DownloadID,
		Bindings:   bindings,
		Cause:      CauseRepair,
		Strategy:   StrategyHistoryFailed,
		Status:     StatusWaitingForImport,
		CreatedAt:  now.Add(-waitingTimeout - time.Minute),
		UpdatedAt:  now.Add(-waitingTimeout - time.Minute),
	}, bindings[0]
}

// wantImportRequests is how many reads judging tc should take: the library,
// then the download's failed and grab history once it looks replaced.
func wantImportRequests(tc importedJobCase) int64 {
	var history int64
	if tc.ready {
		history = 2
	}
	switch {
	case tc.changedInstance:
		return 0
	case tc.kind == arr.Sonarr:
		return 2 + history
	default:
		return 1 + history
	}
}

func TestReconcileImportedManagedJobs(t *testing.T) {
	t.Parallel()
	for _, tc := range []importedJobCase{
		{name: "movie imported outside managed index", kind: arr.Radarr, movie: `{"id":9,"movieFile":{"id":44,"movieId":9,"path":"/library/local.mkv"}}`, ready: true},
		{name: "movie reimported from same download", kind: arr.Radarr, movie: `{"id":9,"movieFile":{"id":44,"movieId":9,"path":"/library/new.mkv"}}`, ready: true, sameDownload: true},
		{name: "original movie file", kind: arr.Radarr, movie: `{"id":9,"movieFile":{"id":42,"movieId":9,"path":"/library/old.mkv"}}`},
		{name: "different movie", kind: arr.Radarr, movie: `{"id":10,"movieFile":{"id":44,"movieId":10,"path":"/library/new.mkv"}}`},
		{name: "missing movie file", kind: arr.Radarr, movie: `{"id":9}`},
		{name: "changed Arr instance", kind: arr.Radarr, movie: `{"id":9,"movieFile":{"id":44,"movieId":9,"path":"/library/new.mkv"}}`, changedInstance: true},
		{name: "Arr request fails", kind: arr.Radarr, unauthorized: true},
		{name: "all episodes imported", kind: arr.Sonarr,
			files:    `[{"id":44,"seriesId":9,"path":"/library/first.mkv"},{"id":45,"seriesId":9,"path":"/library/second.mkv"}]`,
			episodes: `[{"id":101,"episodeFileId":44},{"id":102,"episodeFileId":45}]`, ready: true},
		{name: "partial season import", kind: arr.Sonarr,
			files:    `[{"id":44,"seriesId":9,"path":"/library/first.mkv"},{"id":43,"seriesId":9,"path":"/library/old-second.mkv"}]`,
			episodes: `[{"id":101,"episodeFileId":44},{"id":102,"episodeFileId":43}]`},
		{name: "different episodes", kind: arr.Sonarr,
			files:    `[{"id":44,"seriesId":9,"path":"/library/other.mkv"}]`,
			episodes: `[{"id":103,"episodeFileId":44}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runImportedJobCase(t, tc)
		})
	}
}

// A waiting history_failed job whose download the Arr grabbed again after the
// failure, and imported, is the bad release back: the job is requeued with the
// re-imported file bound, not marked ready. An index that still binds only the
// replaced file keeps it waiting, saying why without restarting its timeout; a
// re-grab the Arr already failed counts as a replacement.
func TestReconcileRequeuesAJobWhoseDownloadCameBack(t *testing.T) {
	t.Parallel()
	const regrabbed = `{"page":1,"totalRecords":2,"records":[` +
		`{"id":50,"eventType":"grabbed","downloadId":"same-download"},` +
		`{"id":40,"eventType":"downloadFailed","downloadId":"same-download"}]}`
	const regrabFailed = `{"page":1,"totalRecords":3,"records":[` +
		`{"id":51,"eventType":"downloadFailed","downloadId":"same-download"},` +
		`{"id":50,"eventType":"grabbed","downloadId":"same-download"},` +
		`{"id":40,"eventType":"downloadFailed","downloadId":"same-download"}]}`
	for _, tc := range []returnedDownloadCase{
		{name: "re-grabbed and indexed", history: regrabbed, indexed: true, want: StatusQueued, wantRequests: 3},
		{name: "re-grabbed, not indexed yet", history: regrabbed, want: StatusWaitingForImport, wantRequests: 3,
			noted: true},
		{name: "re-grab already failed", history: regrabFailed, indexed: true, want: StatusReady, wantRequests: 3},
		{name: "history unreadable", history: "unauthorized", indexed: true, want: StatusWaitingForImport,
			wantRequests: 2},
		{name: "re-grabbed, heuristic binding", history: regrabbed, indexed: true, heuristic: true,
			want: StatusWaitingForImport, wantRequests: 3, noted: true},
		{name: "command search job", history: regrabbed, indexed: true, strategy: StrategyCommandSearch,
			want: StatusReady, wantRequests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runReturnedDownloadCase(t, tc)
		})
	}
}

type returnedDownloadCase struct {
	name         string
	history      string
	indexed      bool
	heuristic    bool
	noted        bool
	strategy     Strategy
	want         Status
	wantRequests int64
}

// runReturnedDownloadCase reconciles a waiting movie job whose Arr now holds
// file 44, with tc's download history and file 44 bound in the index or not.
func runReturnedDownloadCase(t *testing.T, tc returnedDownloadCase) {
	t.Helper()
	var requests atomic.Int64
	server := serveImportedJobCase(t, importedJobCase{
		kind:    arr.Radarr,
		movie:   `{"id":9,"movieFile":{"id":44,"movieId":9,"path":"/library/new.mkv"}}`,
		history: tc.history,
	}, &requests)
	instance := arr.Arr{Name: "library", Type: arr.Radarr, Host: server.URL, Token: "token"}
	registry := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	registry.AddOrUpdate(instance)
	service, err := NewService(ServiceOptions{Directory: t.TempDir(), Arrs: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	job, binding := waitingImportJob(importedJobCase{kind: arr.Radarr}, instance, time.Now())
	if tc.strategy != "" {
		job.Strategy = tc.strategy
	}
	if saveErr := service.jobRepository.Save(job); saveErr != nil {
		t.Fatal(saveErr)
	}
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	if tc.indexed {
		binding.ArrFileID, binding.LibraryPath = 44, "/library/new.mkv"
	}
	if tc.heuristic {
		binding.Confidence = ConfidenceHeuristic
	}
	if upsertErr := service.UpsertBinding(binding); upsertErr != nil {
		t.Fatal(upsertErr)
	}
	service.reconcileImportedJobs(t.Context())
	updated, _ := service.Job(job.ID)
	if updated.Status != tc.want {
		t.Fatalf("status = %s, want %s (last error %q)", updated.Status, tc.want, updated.LastError)
	}
	if got := requests.Load(); got != tc.wantRequests {
		t.Fatalf("requests = %d, want %d", got, tc.wantRequests)
	}
	if tc.want == StatusWaitingForImport {
		if noted := updated.LastError != ""; noted != tc.noted || !updated.UpdatedAt.Equal(job.UpdatedAt) {
			t.Fatalf("last error = %q, updated at moved %v, want noted %v and unmoved",
				updated.LastError, updated.UpdatedAt.Sub(job.UpdatedAt), tc.noted)
		}
	}
	if tc.want != StatusQueued {
		return
	}
	files := make([]int, 0, len(updated.Bindings))
	for _, bound := range updated.Bindings {
		files = append(files, bound.ArrFileID)
	}
	if !slices.Equal(files, []int{42, 44}) || updated.LastError == "" {
		t.Fatalf("bound files = %v, last error = %q", files, updated.LastError)
	}
}

func TestRebindLeavesAJobThatIsNoLongerWaiting(t *testing.T) {
	t.Parallel()
	instance := arr.Arr{Name: "library", Type: arr.Radarr, Host: "http://arr.invalid", Token: "token"}
	service, err := NewService(ServiceOptions{Directory: t.TempDir(), Arrs: newTestArrStorage()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	job, binding := waitingImportJob(importedJobCase{kind: arr.Radarr}, instance, time.Now())
	job.Status = StatusReady
	service.jobs[job.ID] = job
	binding.ArrFileID = 44
	if rebindErr := service.rebindJob(job.ID, []Binding{binding}); rebindErr == nil {
		t.Fatal("rebindJob succeeded on a ready job")
	}
	if got, _ := service.Job(job.ID); got.Status != StatusReady || len(got.Bindings) != 1 {
		t.Fatalf("job = %s with %d bindings, want ready with 1", got.Status, len(got.Bindings))
	}
}

func TestNoteWaitingLeavesAJobThatIsNoLongerWaiting(t *testing.T) {
	t.Parallel()
	instance := arr.Arr{Name: "library", Type: arr.Radarr, Host: "http://arr.invalid", Token: "token"}
	service, err := NewService(ServiceOptions{Directory: t.TempDir(), Arrs: newTestArrStorage()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	job, _ := waitingImportJob(importedJobCase{kind: arr.Radarr}, instance, time.Now())
	job.Status = StatusQueued
	service.jobs[job.ID] = job
	service.noteWaiting(job.ID, "still waiting")
	if got, _ := service.Job(job.ID); got.LastError != "" {
		t.Fatalf("queued job noted %q", got.LastError)
	}
}

// runImportedJobCase judges one waiting job against tc's library state.
func runImportedJobCase(t *testing.T, tc importedJobCase) {
	t.Helper()
	var requests atomic.Int64
	server := serveImportedJobCase(t, tc, &requests)
	instance := arr.Arr{Name: "library", Type: tc.kind, Host: server.URL, Token: "token"}
	registry := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	registry.AddOrUpdate(instance)
	directory := t.TempDir()
	service, err := NewService(ServiceOptions{Directory: directory, Arrs: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	now := time.Now()
	job, binding := waitingImportJob(tc, instance, now)
	service.now = func() time.Time { return now }
	if saveErr := service.jobRepository.Save(job); saveErr != nil {
		t.Fatal(saveErr)
	}
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	if tc.sameDownload {
		replacement := binding
		replacement.EntryID, replacement.EntryFileID, replacement.ArrFileID = "new-entry", "new-file", 44
		if upsertBindingErr := service.UpsertBinding(replacement); upsertBindingErr != nil {
			t.Fatal(upsertBindingErr)
		}
	}
	if before, _ := service.Job(job.ID); !before.Status.waiting() {
		t.Fatalf("job was already %s", before.Status)
	}
	service.reconcileImportedJobs(t.Context())
	updated, _ := service.Job(job.ID)
	if tc.ready && updated.Status != StatusReady || !tc.ready && !updated.Status.waiting() {
		t.Fatalf("status = %s, imported = %v", updated.Status, tc.ready)
	}
	if got, want := requests.Load(), wantImportRequests(tc); got != want {
		t.Fatalf("requests = %d, want %d", got, want)
	}
	assertImportTimeoutOutcome(t, service, directory, registry, job.ID, tc.ready)
}

// assertImportTimeoutOutcome runs maintenance past the waiting timeout: a
// confirmed job stays ready, an unconfirmed one fails, and both survive a restart.
func assertImportTimeoutOutcome(
	t *testing.T,
	service *Service,
	directory string,
	registry *arr.Service,
	jobID string,
	ready bool,
) {
	t.Helper()
	service.maintainJobs()
	updated, _ := service.Job(jobID)
	want := StatusFailed
	if ready {
		want = StatusReady
	}
	if updated.Status != want {
		t.Fatalf("status after timeout = %s, want %s", updated.Status, want)
	}
	if ready && updated.LastError != "" {
		t.Fatalf("completed job error = %q", updated.LastError)
	}
	if closeErr := service.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	reopened := startTestService(t, directory, registry)
	if saved, _ := reopened.Job(jobID); saved.Status != want {
		t.Fatalf("persisted status = %s, want %s", saved.Status, want)
	}
}

func TestBoundLibraryRequestKeepsItsStrategy(t *testing.T) {
	t.Parallel()
	registry := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	registry.AddOrUpdate(arr.Arr{Name: "movies", Type: arr.Radarr, Host: "http://127.0.0.1:1", Token: "token"})
	instance, _ := registry.Get("movies")
	tests := []struct {
		name     string
		strategy Strategy
		want     Strategy
	}{
		{name: "command search", strategy: StrategyCommandSearch, want: StrategyCommandSearch},
		{name: "empty", want: StrategyHistoryFailed},
		{name: "invalid", strategy: "bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service := startTestService(t, t.TempDir(), registry)
			binding := Binding{
				ArrName: instance.Name, ArrType: instance.Type, ArrInstanceFingerprint: instance.Fingerprint(),
				EntryID: "managed-entry", EntryFileID: "file", DownloadID: "download",
				ArrFileID: 42, LibraryPath: "/library/movie.mkv", MovieID: 9, Confidence: ConfidenceExactPath,
			}
			if err := service.UpsertBinding(binding); err != nil {
				t.Fatal(err)
			}
			job, err := service.ReacquireLibraryFile(t.Context(), LibraryRequest{
				ArrName: "movies", ArrFileID: 42, LibraryPath: "/library/movie.mkv", Cause: CauseRepair,
				Strategy: tt.strategy,
			})
			if tt.want == "" {
				if err == nil || len(service.Jobs()) != 0 {
					t.Fatalf("invalid strategy: job=%v err=%v jobs=%d", job, err, len(service.Jobs()))
				}
				return
			}
			if err != nil || job.Strategy != tt.want {
				t.Fatalf("job=%v err=%v, want strategy %q", job, err, tt.want)
			}
		})
	}
}

func TestUnboundLibraryRequestRejectsInvalidStrategy(t *testing.T) {
	t.Parallel()
	fake := &fakeRadarrLibrary{}
	server := httptest.NewServer(fake)
	defer server.Close()
	registry := arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	registry.AddOrUpdate(arr.Arr{Name: "movies", Type: arr.Radarr, Host: server.URL, Token: "token"})
	service := startTestService(t, t.TempDir(), registry)
	job, err := service.ReacquireLibraryFile(t.Context(), LibraryRequest{
		ArrName: "movies", ArrFileID: 42, LibraryPath: "/library/movie.mkv", Cause: CauseRepair, Strategy: "bogus",
	})
	if err == nil || len(service.Jobs()) != 0 {
		t.Fatalf("job=%v err=%v jobs=%d", job, err, len(service.Jobs()))
	}
}
