package reacquire

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
