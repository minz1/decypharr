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

	"github.com/sirrobot01/decypharr/pkg/arr"
)

// failDownloadArr serves a history that holds a grab for DOWNLOADID, plus a
// download-failed record when alreadyFailed is set.
func failDownloadArr(t *testing.T, grab, alreadyFailed bool, failCalls *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			var records []string
			if grab {
				records = append(records, `{"id":42,"downloadId":"DOWNLOADID","eventType":"grabbed"}`)
			}
			if alreadyFailed {
				records = append(records, `{"id":43,"downloadId":"DOWNLOADID","eventType":"downloadFailed"}`)
			}
			_, _ = fmt.Fprintf(w, `{"page":1,"totalRecords":%d,"records":[%s]}`,
				len(records), strings.Join(records, ","))
		case request.Method == http.MethodPost && request.URL.Path == "/api/v3/history/failed/42":
			failCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// startFailDownloadService serves jobs against host; an empty host installs
// no handler, so queued jobs stay queued.
func startFailDownloadService(t *testing.T, host string) *Service {
	t.Helper()
	options := ServiceOptions{Directory: t.TempDir()}
	if host != "" {
		registry := newTestArrStorage()
		registry.AddOrUpdate(arr.Arr{Name: "sonarr", Host: host, Token: "secret", Type: arr.Sonarr})
		options.Handler = NewHandler(registry, nil)
	}
	service, err := NewService(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	return service
}

func TestFailDownloadFailsGrabOnce(t *testing.T) {
	t.Parallel()
	var failCalls atomic.Int64
	service := startFailDownloadService(t, failDownloadArr(t, true, false, &failCalls).URL)
	job, err := service.FailDownload("sonarr", "DOWNLOADID", "entry")
	if err != nil {
		t.Fatal(err)
	}
	waitForJobStatus(t, service, job.ID, StatusReady)
	if got := failCalls.Load(); got != 1 {
		t.Fatalf("FailHistory calls = %d, want 1", got)
	}
}

func TestFailDownloadSkipsAlreadyFailedGrab(t *testing.T) {
	t.Parallel()
	var failCalls atomic.Int64
	service := startFailDownloadService(t, failDownloadArr(t, true, true, &failCalls).URL)
	job, err := service.FailDownload("sonarr", "DOWNLOADID", "entry")
	if err != nil {
		t.Fatal(err)
	}
	waitForJobStatus(t, service, job.ID, StatusReady)
	if got := failCalls.Load(); got != 0 {
		t.Fatalf("FailHistory calls = %d, want 0", got)
	}
}

func TestFailDownloadWithoutGrabFails(t *testing.T) {
	t.Parallel()
	var failCalls atomic.Int64
	service := startFailDownloadService(t, failDownloadArr(t, false, false, &failCalls).URL)
	job, err := service.FailDownload("sonarr", "DOWNLOADID", "entry")
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForJobStatus(t, service, job.ID, StatusFailed)
	if !strings.Contains(failed.LastError, "no grab history") {
		t.Fatalf("LastError = %q", failed.LastError)
	}
}

func TestFailDownloadDeduplicates(t *testing.T) {
	t.Parallel()
	service := startFailDownloadService(t, "")
	first, err := service.FailDownload("sonarr", "ABC", "abc")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.FailDownload("sonarr", "ABC", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("jobs %q and %q, want one", first.ID, second.ID)
	}
	if _, emptyErr := service.FailDownload("", "ABC", "abc"); emptyErr == nil {
		t.Fatal("empty arr name accepted")
	}
}

func TestDownloadFailedJobRoundTrips(t *testing.T) {
	t.Parallel()
	service := startFailDownloadService(t, "")
	job, err := service.FailDownload("sonarr", "ABC", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if validateErr := validateJob(*job); validateErr != nil {
		t.Fatal(validateErr)
	}
	noID := *job
	noID.DownloadID = ""
	if validateErr := validateJob(noID); validateErr == nil {
		t.Fatal("download_failed job without download ID accepted")
	}
	other := *job
	other.Strategy = StrategyHistoryFailed
	if validateErr := validateJob(other); validateErr == nil {
		t.Fatal("bindingless job with another strategy accepted")
	}
}

// A history lookup that fails before any mutation is sent marks the job
// retryable rather than failed: the Arr may only be restarting.
func TestFailDownloadLookupErrorIsRetryable(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(down.Close)
	registry := newTestArrStorage()
	registry.AddOrUpdate(arr.Arr{Name: "sonarr", Host: down.URL, Token: "secret", Type: arr.Sonarr})
	handler, _ := NewHandler(registry, nil).(*arrHandler)
	job := Job{ArrName: "sonarr", DownloadID: "DOWNLOADID", Strategy: StrategyDownloadFailed}
	err := handler.failDownload(t.Context(), job, nil)
	if !errors.Is(err, errArrLookup) {
		t.Fatalf("failDownload error = %v, want errArrLookup", err)
	}
}

// settleJob requeues an unavailable-Arr failure until the reconciliation
// deadline, then stops for an operator instead of failing silently.
func TestSettleJobRetriesFailedLookupUntilDeadline(t *testing.T) {
	t.Parallel()
	service := startFailDownloadService(t, "")
	queued, err := service.FailDownload("sonarr", "DOWNLOADID", "entry")
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.updateJob(queued.ID, StatusResolving, nil)
	if err != nil {
		t.Fatal(err)
	}
	progress := &serviceJobProgress{service: service, jobID: started.ID}
	lookupErr := fmt.Errorf("%w: connection refused", errArrLookup)

	service.settleJob(started.ID, lookupErr, progress)
	retried, _ := service.Job(started.ID)
	if retried.Status != StatusQueued || retried.RetryAt.Before(started.UpdatedAt.Add(retryMaxDelay)) {
		t.Fatalf("after a lookup failure: status %q retryAt %v, want queued at least %v later",
			retried.Status, retried.RetryAt, retryMaxDelay)
	}

	service.now = func() time.Time { return started.StartedAt.Add(reconciliationTimeout + time.Minute) }
	service.settleJob(started.ID, lookupErr, progress)
	stopped, _ := service.Job(started.ID)
	if stopped.Status != StatusNeedsAttention {
		t.Fatalf("after the deadline: status %q, want %q", stopped.Status, StatusNeedsAttention)
	}
}

type recordingInvalidator struct{ jobs []Job }

func (r *recordingInvalidator) InvalidateReacquire(_ context.Context, job Job) error {
	r.jobs = append(r.jobs, job)
	return nil
}

// After the Arr confirms the failed grab, the download's own queue record has
// nothing left to report: the job hands it to the invalidator so the Arr's
// queue stops tracking it. A job that finds no grab leaves it alone.
func TestFailDownloadInvalidatesOnlyAfterFailingTheGrab(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		grab, alreadyFailed bool
		want                int
	}{
		"grab failed":         {grab: true, want: 1},
		"grab already failed": {grab: true, alreadyFailed: true, want: 1},
		"no grab":             {want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var failCalls atomic.Int64
			server := failDownloadArr(t, tc.grab, tc.alreadyFailed, &failCalls)
			registry := newTestArrStorage()
			registry.AddOrUpdate(arr.Arr{Name: "sonarr", Host: server.URL, Token: "secret", Type: arr.Sonarr})
			invalidator := &recordingInvalidator{}
			handler, _ := NewHandler(registry, invalidator).(*arrHandler)
			job := Job{
				ArrName:    "sonarr",
				DownloadID: "DOWNLOADID",
				EntryID:    "downloadid",
				Strategy:   StrategyDownloadFailed,
			}
			_ = handler.failDownload(t.Context(), job, &recordedProgress{})
			if got := len(invalidator.jobs); got != tc.want {
				t.Fatalf("invalidations = %d, want %d", got, tc.want)
			}
		})
	}
}

// Failing a grab is for downloads that never reached the library; one the arr imported must be left alone.
func TestFailDownloadSkipsImportedDownload(t *testing.T) {
	t.Parallel()
	var failCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":2,"records":[`+
				`{"id":44,"downloadId":"DOWNLOADID","eventType":"downloadFolderImported"},`+
				`{"id":42,"downloadId":"DOWNLOADID","eventType":"grabbed"}]}`)
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/v3/history/failed/"):
			failCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	registry := newTestArrStorage()
	registry.AddOrUpdate(arr.Arr{Name: "sonarr", Host: server.URL, Token: "secret", Type: arr.Sonarr})
	invalidator := &recordingInvalidator{}
	service, err := NewService(ServiceOptions{Directory: t.TempDir(), Handler: NewHandler(registry, invalidator)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	job, err := service.FailDownload("sonarr", "DOWNLOADID", "entry")
	if err != nil {
		t.Fatal(err)
	}
	waitForJobStatus(t, service, job.ID, StatusReady)
	if got := failCalls.Load(); got != 0 {
		t.Fatalf("FailHistory calls = %d, want 0 for an imported download", got)
	}
	if len(invalidator.jobs) != 0 {
		t.Fatalf("invalidated %v, want none: the queue record backs imported files", invalidator.jobs)
	}
}

// A dead hash grabbed again after an earlier failure must be failed again; the old failure covers only the old grab.
func TestFailDownloadFailsARegrabOfAFailedHash(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		failed []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			records := `{"id":50,"downloadId":"DOWNLOADID","eventType":"grabbed"},` +
				`{"id":43,"downloadId":"DOWNLOADID","eventType":"downloadFailed"},` +
				`{"id":42,"downloadId":"DOWNLOADID","eventType":"grabbed"}`
			if len(failed) > 0 {
				records = `{"id":51,"downloadId":"DOWNLOADID","eventType":"downloadFailed"},` + records
			}
			total := strings.Count(records, `"id"`)
			_, _ = fmt.Fprintf(w, `{"page":1,"totalRecords":%d,"records":[%s]}`, total, records)
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/v3/history/failed/"):
			failed = append(failed, strings.TrimPrefix(request.URL.Path, "/api/v3/history/failed/"))
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	service := startFailDownloadService(t, server.URL)
	job, err := service.FailDownload("sonarr", "DOWNLOADID", "entry")
	if err != nil {
		t.Fatal(err)
	}
	waitForJobStatus(t, service, job.ID, StatusReady)
	mu.Lock()
	defer mu.Unlock()
	if len(failed) != 1 || failed[0] != "50" {
		t.Fatalf("failed history ids = %v, want the re-grab 50", failed)
	}
}

// An import from an earlier grab of the same hash does not protect a dead re-grab.
func TestFailDownloadIgnoresAnImportFromAnOlderGrab(t *testing.T) {
	t.Parallel()
	var failCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":3,"records":[`+
				`{"id":60,"downloadId":"DOWNLOADID","eventType":"grabbed"},`+
				`{"id":44,"downloadId":"DOWNLOADID","eventType":"downloadFolderImported"},`+
				`{"id":42,"downloadId":"DOWNLOADID","eventType":"grabbed"}]}`)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v3/history/failed/60":
			failCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	service := startFailDownloadService(t, server.URL)
	job, err := service.FailDownload("sonarr", "DOWNLOADID", "entry")
	if err != nil {
		t.Fatal(err)
	}
	waitForJobStatus(t, service, job.ID, StatusReady)
	if got := failCalls.Load(); got != 1 {
		t.Fatalf("FailHistory calls on the re-grab = %d, want 1", got)
	}
}

func TestFailDownloadQueriesHistoryInTheArrsStoredForm(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, jobID, storedID string }{
		{name: "torrent infohash stored uppercase", jobID: "8eae8734c0ffee00c0ffee00c0ffee00c0ff8844", storedID: "8EAE8734C0FFEE00C0FFEE00C0FFEE00C0FF8844"},
		{name: "usenet id stored as given", jobID: "9fdb8e00-5a38-4c3e-9d1a-0123456789ab", storedID: "9fdb8e00-5a38-4c3e-9d1a-0123456789ab"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var failCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
					if request.URL.Query().Get("downloadId") != test.storedID {
						_, _ = fmt.Fprint(w, `{"page":1,"totalRecords":0,"records":[]}`)
						return
					}
					_, _ = fmt.Fprintf(w,
						`{"page":1,"totalRecords":1,"records":[{"id":42,"downloadId":%q,"eventType":"grabbed"}]}`,
						test.storedID)
				case request.Method == http.MethodPost && request.URL.Path == "/api/v3/history/failed/42":
					failCalls.Add(1)
					w.WriteHeader(http.StatusOK)
				default:
					http.NotFound(w, request)
				}
			}))
			t.Cleanup(server.Close)

			registry := newTestArrStorage()
			registry.AddOrUpdate(arr.Arr{Name: "sonarr", Host: server.URL, Token: "secret", Type: arr.Sonarr})
			handler := NewHandler(registry, nil).(*arrHandler)
			job := Job{ArrName: "sonarr", ArrType: arr.Sonarr, DownloadID: test.jobID}
			if err := handler.failDownload(t.Context(), job, &recordedProgress{}); err != nil {
				t.Fatal(err)
			}
			if got := failCalls.Load(); got != 1 {
				t.Fatalf("FailHistory calls = %d, want 1", got)
			}
		})
	}
}

// A provider rejecting the Arr's re-grab of a download a waiting job already failed reruns that job, so the re-grab
// is failed too instead of idling to the waiting timeout.
func TestFailDownloadRerunsAWaitingJobForTheDownload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		strategy Strategy
		reruns   bool
	}{
		{strategy: StrategyHistoryFailed, reruns: true},
		{strategy: StrategyCommandSearch},
	} {
		t.Run(string(tc.strategy), func(t *testing.T) {
			t.Parallel()
			runWaitingJobFailDownload(t, tc.strategy, tc.reruns)
		})
	}
}

// runWaitingJobFailDownload fails the download of a waiting job with strategy and checks whether it reruns.
func runWaitingJobFailDownload(t *testing.T, strategy Strategy, reruns bool) {
	t.Helper()
	ran := make(chan string, 1)
	handler := reconciliationHandler(func(_ context.Context, job Job, progress JobProgress) error {
		ran <- job.ID
		return progress.Update(StatusWaitingForGrab, nil)
	})
	instance := arr.Arr{Name: "sonarr", Type: arr.Sonarr, Host: "http://arr.invalid", Token: "token"}
	service, err := NewService(ServiceOptions{Directory: t.TempDir(), Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	waiting, _ := waitingImportJob(importedJobCase{kind: arr.Sonarr}, instance, time.Now())
	waiting.Strategy = strategy
	waiting.CreatedAt, waiting.UpdatedAt = time.Now(), time.Now()
	if saveErr := service.jobRepository.Save(waiting); saveErr != nil {
		t.Fatal(saveErr)
	}
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	job, err := service.FailDownload("sonarr", waiting.DownloadID, "entry")
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != waiting.ID {
		t.Fatalf("FailDownload returned job %q, want the waiting job %q", job.ID, waiting.ID)
	}
	select {
	case id := <-ran:
		if !reruns {
			t.Fatalf("job %q reran", id)
		}
	case <-time.After(200 * time.Millisecond):
		if reruns {
			t.Fatal("waiting job did not rerun")
		}
		if got, _ := service.Job(waiting.ID); got.Status != StatusWaitingForImport {
			t.Fatalf("status = %s, want it still waiting for import", got.Status)
		}
	}
}

func TestFailDownloadReturnsARunningHistoryFailedJob(t *testing.T) {
	t.Parallel()
	running, finish := make(chan struct{}), make(chan struct{})
	handler := reconciliationHandler(func(_ context.Context, _ Job, progress JobProgress) error {
		close(running)
		<-finish
		return progress.Update(StatusWaitingForGrab, nil)
	})
	instance := arr.Arr{Name: "sonarr", Type: arr.Sonarr, Host: "http://arr.invalid", Token: "token"}
	service, err := NewService(ServiceOptions{Directory: t.TempDir(), Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	t.Cleanup(func() { close(finish) })
	job, _ := waitingImportJob(importedJobCase{kind: arr.Sonarr}, instance, time.Now())
	job.Status = StatusQueued
	job.CreatedAt, job.UpdatedAt = time.Now(), time.Now()
	if saveErr := service.jobRepository.Save(job); saveErr != nil {
		t.Fatal(saveErr)
	}
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("job never started")
	}
	got, err := service.FailDownload("sonarr", job.DownloadID, "entry")
	if err != nil {
		t.Fatalf("FailDownload on a running job: %v", err)
	}
	if got.ID != job.ID {
		t.Fatalf("FailDownload returned job %q, want the running job %q", got.ID, job.ID)
	}
}
