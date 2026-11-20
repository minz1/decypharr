package reacquire

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

var errInjectedSync = errors.New("injected sync failure")

var errInjectedPut = errors.New("injected put failure")

type observedJobStore struct {
	jobStore

	mu       sync.Mutex
	puts     []string
	syncs    int
	putErr   error
	syncErrs []error
}

func (s *observedJobStore) Put(key string, value []byte, options *appendstore.PutOptions) error {
	s.mu.Lock()
	err := s.putErr
	s.putErr = nil
	if err == nil {
		s.puts = append(s.puts, key)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.jobStore.Put(key, value, options)
}

func (s *observedJobStore) Sync() error {
	s.mu.Lock()
	s.syncs++
	var err error
	if len(s.syncErrs) > 0 {
		err = s.syncErrs[0]
		s.syncErrs = s.syncErrs[1:]
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.jobStore.Sync()
}

func (s *observedJobStore) counts() ([]string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...), s.syncs
}

func (s *observedJobStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts, s.syncs = nil, 0
}

type seededService struct {
	*Service

	store *observedJobStore
}

func seedFailService(t *testing.T, directory string, seed ...Job) seededService {
	t.Helper()
	service, err := NewService(newTestGroup(t), ServiceOptions{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	for _, job := range seed {
		if saveErr := service.jobRepository.Save(job); saveErr != nil {
			t.Fatal(saveErr)
		}
	}
	store := &observedJobStore{jobStore: service.jobRepository.store}
	service.jobRepository.store = store
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	return seededService{Service: service, store: store}
}

const receiptDownloadID = "same-download"

func receiptInstance() arr.Arr {
	return arr.Arr{Name: "sonarr", Type: arr.Sonarr, Host: "http://arr.invalid", Token: "token"}
}

func receiptJob(id string, strategy Strategy, status Status, createdAt time.Time) Job {
	job := Job{
		ID:         id,
		Status:     status,
		Cause:      CauseRepair,
		Strategy:   strategy,
		ArrName:    "sonarr",
		EntryID:    "OtherEntry",
		DownloadID: receiptDownloadID,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	}
	if status.Terminal() {
		job.CompletedAt = createdAt
	}
	if strategy != StrategyDownloadFailed {
		waiting, _ := waitingImportJob(importedJobCase{kind: arr.Sonarr}, receiptInstance(), createdAt)
		job.Bindings = waiting.Bindings
		job.FileID = waiting.FileID
		job.EntryID = waiting.EntryID
		job.ArrType = waiting.ArrType
	}
	return job
}

type failDecisionCase struct {
	name      string
	seed      []Job
	since     time.Time
	handed    Handed
	sameID    string
	otherJob  Strategy
	status    Status
	wantCount int
}

func TestFailDownloadDecision(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	before := since.Add(-time.Hour)
	for _, tc := range []failDecisionCase{
		{
			name:      "nonterminal download_failed of another entry is a receipt",
			seed:      []Job{receiptJob("j1", StrategyDownloadFailed, StatusQueued, before)},
			since:     since,
			handed:    HandedExisting,
			sameID:    "j1",
			wantCount: 1,
		},
		{
			name:      "nonterminal history_failed is a receipt",
			seed:      []Job{receiptJob("j1", StrategyHistoryFailed, StatusQueued, before)},
			since:     since,
			handed:    HandedExisting,
			sameID:    "j1",
			wantCount: 1,
		},
		{
			name:      "waiting history_failed is requeued",
			seed:      []Job{receiptJob("j1", StrategyHistoryFailed, StatusWaitingForImport, before)},
			since:     since,
			handed:    HandedRequeued,
			sameID:    "j1",
			status:    StatusQueued,
			wantCount: 1,
		},
		{
			name:      "waiting command_search is refused",
			seed:      []Job{receiptJob("j1", StrategyCommandSearch, StatusWaitingForImport, before)},
			since:     since,
			otherJob:  StrategyCommandSearch,
			wantCount: 1,
		},
		{
			name:      "queued command_search is refused",
			seed:      []Job{receiptJob("j1", StrategyCommandSearch, StatusQueued, before)},
			since:     since,
			otherJob:  StrategyCommandSearch,
			wantCount: 1,
		},
		{
			name:      "terminal created before since is not a receipt",
			seed:      []Job{receiptJob("j1", StrategyDownloadFailed, StatusReady, before)},
			since:     since,
			handed:    HandedCreated,
			wantCount: 2,
		},
		{
			name:      "terminal created at since is a receipt",
			seed:      []Job{receiptJob("j1", StrategyDownloadFailed, StatusReady, since)},
			since:     since,
			handed:    HandedExisting,
			sameID:    "j1",
			wantCount: 1,
		},
		{
			name:      "terminal history_failed created after since is a receipt",
			seed:      []Job{receiptJob("j1", StrategyHistoryFailed, StatusReady, since.Add(time.Minute))},
			since:     since,
			handed:    HandedExisting,
			sameID:    "j1",
			wantCount: 1,
		},
		{
			name:      "a zero since never makes a terminal job a receipt",
			seed:      []Job{receiptJob("j1", StrategyDownloadFailed, StatusReady, before)},
			handed:    HandedCreated,
			wantCount: 2,
		},
		{
			name:      "no job creates one",
			since:     since,
			handed:    HandedCreated,
			wantCount: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

func (tc failDecisionCase) run(t *testing.T) {
	service := seedFailService(t, t.TempDir(), tc.seed...)
	job, handed, err := service.FailDownload("sonarr", receiptDownloadID, "entry", tc.since)
	if tc.otherJob != "" {
		var other *OtherJobError
		if !errors.As(err, &other) || other.Strategy != tc.otherJob {
			t.Fatalf("error = %v, want an OtherJobError for %s", err, tc.otherJob)
		}
	} else {
		tc.checkHanded(t, job, handed, err)
	}
	if got := len(service.Jobs()); got != tc.wantCount {
		t.Fatalf("jobs = %d, want %d", got, tc.wantCount)
	}
}

func (tc failDecisionCase) checkHanded(t *testing.T, job Job, handed Handed, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if handed != tc.handed {
		t.Fatalf("handed = %v, want %v", handed, tc.handed)
	}
	if tc.sameID != "" && job.ID != tc.sameID {
		t.Fatalf("job %q, want %q", job.ID, tc.sameID)
	}
	if tc.sameID == "" && (job.ID == "" || len(tc.seed) > 0 && job.ID == tc.seed[0].ID) {
		t.Fatalf("job %q, want a new job", job.ID)
	}
	if tc.status != "" && job.Status != tc.status {
		t.Fatalf("status = %s, want %s", job.Status, tc.status)
	}
}

func TestFailDownloadSyncs(t *testing.T) {
	t.Parallel()
	before := time.Now().Add(-time.Hour).UTC()
	for _, tc := range []struct {
		name   string
		seed   []Job
		handed Handed
	}{
		{name: "created", handed: HandedCreated},
		{
			name:   "requeued",
			seed:   []Job{receiptJob("j1", StrategyHistoryFailed, StatusWaitingForImport, before)},
			handed: HandedRequeued,
		},
		{
			name:   "existing",
			seed:   []Job{receiptJob("j1", StrategyDownloadFailed, StatusQueued, before)},
			handed: HandedExisting,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service := seedFailService(t, t.TempDir(), tc.seed...)
			service.store.reset()
			_, handed, err := service.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{})
			if err != nil || handed != tc.handed {
				t.Fatalf("handed = %v, err = %v, want %v", handed, err, tc.handed)
			}
			if _, syncs := service.store.counts(); syncs != 1 {
				t.Fatalf("syncs = %d, want exactly 1", syncs)
			}
		})
	}
}

func TestFailDownloadStagedUntilSynced(t *testing.T) {
	t.Parallel()
	t.Run("put failure publishes nothing", stagedPutFailure)
	t.Run("sync failure on create stages the job", stagedCreateSyncFailure)
	t.Run("sync failure on requeue keeps the job waiting", stagedRequeueSyncFailure)
}

func stagedPutFailure(t *testing.T) {
	t.Parallel()
	service := seedFailService(t, t.TempDir())
	service.store.putErr = errInjectedPut
	_, _, err := service.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{})
	if !errors.Is(err, errInjectedPut) {
		t.Fatalf("error = %v, want the put error", err)
	}
	if len(service.Jobs()) != 0 {
		t.Fatalf("jobs = %v, want none", service.Jobs())
	}
	if _, found := service.FailJob("sonarr", receiptDownloadID, "entry", time.Time{}); found {
		t.Fatal("FailJob found a job that was never stored")
	}
	if _, _, retryErr := service.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{}); retryErr != nil {
		t.Fatal(retryErr)
	}
}

func stagedCreateSyncFailure(t *testing.T) {
	t.Parallel()
	key := jobKey{arrName: "sonarr", downloadID: receiptDownloadID}
	service := seedFailService(t, t.TempDir())
	service.store.syncErrs = []error{errInjectedSync}
	_, _, err := service.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{})
	if !errors.Is(err, errInjectedSync) {
		t.Fatalf("error = %v, want the sync error", err)
	}
	if len(service.Jobs()) != 0 {
		t.Fatalf("jobs = %v, want none published", service.Jobs())
	}
	if _, runnable := service.nextJob(); runnable {
		t.Fatal("the worker can see a job whose sync failed")
	}
	if _, found := service.FailJob("sonarr", receiptDownloadID, "entry", time.Time{}); found {
		t.Fatal("FailJob reported a staged job as a receipt")
	}
	service.jobsMu.RLock()
	staged, isStaged := service.staged[key]
	service.jobsMu.RUnlock()
	if !isStaged {
		t.Fatal("the job is not staged")
	}

	job, handed, err := service.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{})
	if err != nil || handed != HandedCreated {
		t.Fatalf("retry: handed = %v, err = %v", handed, err)
	}
	if job.ID != staged.ID {
		t.Fatalf("retry job %q, want the staged %q", job.ID, staged.ID)
	}
	if jobs := service.Jobs(); len(jobs) != 1 || jobs[0].ID != staged.ID {
		t.Fatalf("jobs = %v, want only %q", jobs, staged.ID)
	}
	if _, found := service.FailJob("sonarr", receiptDownloadID, "entry", time.Time{}); !found {
		t.Fatal("FailJob misses the published job")
	}
	service.jobsMu.RLock()
	left := len(service.staged)
	service.jobsMu.RUnlock()
	if left != 0 {
		t.Fatalf("staged = %d after publish, want 0", left)
	}
}

func stagedRequeueSyncFailure(t *testing.T) {
	t.Parallel()
	waiting := receiptJob("j1", StrategyHistoryFailed, StatusWaitingForImport, time.Now().Add(-time.Hour).UTC())
	service := seedFailService(t, t.TempDir(), waiting)
	service.store.syncErrs = []error{errInjectedSync}
	_, _, err := service.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{})
	if !errors.Is(err, errInjectedSync) {
		t.Fatalf("error = %v, want the sync error", err)
	}
	if got, _ := service.Job("j1"); got.Status != StatusWaitingForImport {
		t.Fatalf("published status = %s, want it still waiting", got.Status)
	}
	if _, runnable := service.nextJob(); runnable {
		t.Fatal("the worker can see a requeue whose sync failed")
	}
	job, handed, err := service.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{})
	if err != nil || handed != HandedRequeued || job.ID != "j1" || job.Status != StatusQueued {
		t.Fatalf("retry: job %q %s handed %v err %v", job.ID, job.Status, handed, err)
	}
}

func TestFailDownloadStagedRestart(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	first, err := NewService(newTestGroup(t), ServiceOptions{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	store := &observedJobStore{jobStore: first.jobRepository.store, syncErrs: []error{errInjectedSync}}
	first.jobRepository.store = store
	if startErr := first.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	_, _, failErr := first.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{})
	if !errors.Is(failErr, errInjectedSync) {
		t.Fatalf("error = %v, want the sync error", failErr)
	}
	puts, _ := store.counts()
	if len(puts) != 1 {
		t.Fatalf("puts = %v, want one landed row", puts)
	}
	if closeErr := first.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	second := seedFailService(t, directory)
	jobs := second.Jobs()
	if len(jobs) != 1 || jobs[0].ID != puts[0] || jobs[0].Status != StatusQueued {
		t.Fatalf("reloaded jobs = %v, want the landed row %q queued", jobs, puts[0])
	}
	job, handed, err := second.FailDownload("sonarr", receiptDownloadID, "entry", time.Time{})
	if err != nil || handed != HandedExisting || job.ID != puts[0] {
		t.Fatalf("after restart: job %q handed %v err %v, want existing %q", job.ID, handed, err, puts[0])
	}
}

type failJobCase struct {
	name       string
	seed       Job
	arr        string
	downloadID string
	entryID    string
	want       bool
}

func TestFailJobReceiptRule(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(-time.Hour).UTC()
	before := since.Add(-time.Hour)
	for _, tc := range []failJobCase{
		{name: "nonterminal download_failed", seed: receiptJob("j", StrategyDownloadFailed, StatusQueued, before), want: true},
		{name: "nonterminal history_failed", seed: receiptJob("j", StrategyHistoryFailed, StatusQueued, before), want: true},
		{name: "terminal at since", seed: receiptJob("j", StrategyDownloadFailed, StatusReady, since), want: true},
		{name: "terminal before since", seed: receiptJob("j", StrategyDownloadFailed, StatusReady, before)},
		{name: "waiting history_failed", seed: receiptJob("j", StrategyHistoryFailed, StatusWaitingForImport, before)},
		{name: "other strategy", seed: receiptJob("j", StrategyCommandSearch, StatusQueued, before)},
		{name: "other arr", seed: receiptJob("j", StrategyDownloadFailed, StatusQueued, before), arr: "radarr"},
		{
			name: "other download", seed: receiptJob("j", StrategyDownloadFailed, StatusQueued, before),
			downloadID: "elsewhere",
		},
		{
			name: "entry id case differs", seed: receiptJob("j", StrategyDownloadFailed, StatusQueued, before),
			entryID: "OTHERENTRY", want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t, since)
		})
	}
}

func (tc failJobCase) run(t *testing.T, since time.Time) {
	service := seedFailService(t, t.TempDir(), tc.seed)
	arrName, downloadID, entryID := "sonarr", receiptDownloadID, "otherentry"
	if tc.arr != "" {
		arrName = tc.arr
	}
	if tc.downloadID != "" {
		downloadID = tc.downloadID
	}
	if tc.entryID != "" {
		entryID = tc.entryID
	}
	service.store.reset()
	job, found := service.FailJob(arrName, downloadID, entryID, since)
	if found != tc.want {
		t.Fatalf("FailJob = %v, want %v", found, tc.want)
	}
	if found && job.ID != "j" {
		t.Fatalf("job %q, want j", job.ID)
	}
	if puts, syncs := service.store.counts(); len(puts) != 0 || syncs != 0 {
		t.Fatalf("FailJob wrote: puts %v syncs %d", puts, syncs)
	}
}

type failOnceInvalidator struct {
	calls atomic.Int64
}

func (i *failOnceInvalidator) InvalidateReacquire(context.Context, Job) error {
	if i.calls.Add(1) == 1 {
		return &RetryableInvalidateError{Err: errors.New("remove record: disk full")}
	}
	return nil
}

func TestRetryableInvalidateErrorRequeues(t *testing.T) {
	t.Parallel()
	t.Run("settleJob requeues with a delay and counts no mutation", retryableSettleRequeues)
	t.Run("the rerun makes no second Arr mutation", retryableRerunMutatesOnce)
}

func retryableSettleRequeues(t *testing.T) {
	t.Parallel()
	service := startFailDownloadService(t, "")
	queued, _, err := service.FailDownload("sonarr", "DOWNLOADID", "entry", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.updateJob(queued.ID, StatusResolving, nil)
	if err != nil {
		t.Fatal(err)
	}
	progress := &serviceJobProgress{service: service, jobID: started.ID}
	retryable := fmt.Errorf("invalidate: %w", &RetryableInvalidateError{Err: errors.New("disk full")})
	if !service.settleJob(t.Context(), nil, started.ID, retryable, progress) {
		t.Fatal("settleJob reported the job could not be updated")
	}
	got, _ := service.Job(started.ID)
	if got.Status != StatusQueued || got.RetryAt.Before(started.UpdatedAt.Add(retryMaxDelay)) {
		t.Fatalf("status %q retryAt %v, want queued at least %v later", got.Status, got.RetryAt, retryMaxDelay)
	}
	if len(got.Mutations) != 0 || !strings.Contains(got.LastError, "disk full") {
		t.Fatalf("mutations %v lastError %q", got.Mutations, got.LastError)
	}
}

func retryableRerunMutatesOnce(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var posts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			records := `{"id":42,"downloadId":"DOWNLOADID","eventType":"grabbed"}`
			if len(posts) > 0 {
				records = `{"id":43,"downloadId":"DOWNLOADID","eventType":"downloadFailed"},` + records
			}
			_, _ = fmt.Fprintf(
				w,
				`{"page":1,"totalRecords":%d,"records":[%s]}`,
				strings.Count(records, `"id"`),
				records,
			)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v3/history/failed/42":
			posts = append(posts, request.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	registry := newTestArrStorage()
	registry.AddOrUpdate(arr.Arr{Name: "sonarr", Host: server.URL, Token: "secret", Type: arr.Sonarr})
	handler, _ := NewHandler(registry, &failOnceInvalidator{}).(*arrHandler)
	job := Job{ArrName: "sonarr", DownloadID: "DOWNLOADID", Strategy: StrategyDownloadFailed}
	err := handler.failDownload(t.Context(), job, &recordedProgress{})
	if _, retryable := errors.AsType[*RetryableInvalidateError](err); !retryable {
		t.Fatalf("first run error = %v, want a RetryableInvalidateError", err)
	}
	if rerunErr := handler.failDownload(t.Context(), job, &recordedProgress{}); rerunErr != nil {
		t.Fatalf("rerun: %v", rerunErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 1 {
		t.Fatalf("Arr fail posts = %v, want exactly one across both runs", posts)
	}
}

func TestKeyReleasedHookFires(t *testing.T) {
	t.Parallel()
	t.Run("a terminal transition", func(t *testing.T) {
		t.Parallel()
		service := startFailDownloadService(t, "")
		type release struct{ arr, download string }
		released := make(chan release, 4)
		service.SetKeyReleasedHook(func(arrName, downloadID string) {
			_ = service.Jobs()
			released <- release{arrName, downloadID}
		})
		job, _, err := service.FailDownload("sonarr", "ABC", "abc", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if _, updateErr := service.updateJob(job.ID, StatusFailed, nil); updateErr != nil {
			t.Fatal(updateErr)
		}
		select {
		case got := <-released:
			if got != (release{"sonarr", "ABC"}) {
				t.Fatalf("released %v", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("hook never fired")
		}
		if len(released) != 0 {
			t.Fatal("hook fired more than once")
		}
	})

	t.Run("the reset clear", func(t *testing.T) {
		t.Parallel()
		service := startFailDownloadService(t, "")
		released := make(chan string, 4)
		service.SetKeyReleasedHook(func(_, downloadID string) { released <- downloadID })
		if _, _, err := service.FailDownload("sonarr", "ABC", "abc", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := service.loadJobs(nil); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-released:
			if got != "ABC" {
				t.Fatalf("released %q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("hook never fired for the cleared key")
		}
	})
}
