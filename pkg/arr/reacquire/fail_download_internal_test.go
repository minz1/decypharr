package reacquire

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

// failDownloadArr serves a history that holds a grab for DOWNLOADID, plus a
// download-failed record when alreadyFailed is set.
func failDownloadArr(t *testing.T, grab, alreadyFailed bool, failCalls *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v3/history":
			records := ""
			if grab {
				records = `{"id":42,"downloadId":"DOWNLOADID","eventType":"grabbed"}`
			}
			if alreadyFailed {
				records += `,{"id":43,"downloadId":"DOWNLOADID","eventType":"downloadFailed"}`
			}
			if records != "" && records[0] == ',' {
				records = records[1:]
			}
			n := 0
			if records != "" {
				n = 1 + btoi(alreadyFailed && grab)
			}
			_, _ = fmt.Fprintf(w, `{"page":1,"totalRecords":%d,"records":[%s]}`, n, records)
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

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func startFailDownloadService(t *testing.T, host string) *Service {
	t.Helper()
	registry := newTestArrStorage()
	registry.AddOrUpdate(arr.Arr{Name: "sonarr", Host: host, Token: "secret", Type: arr.Sonarr})
	service, err := NewService(ServiceOptions{Directory: t.TempDir(), Handler: NewHandler(registry, nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if startErr := service.Start(t.Context()); startErr != nil {
		t.Fatal(startErr)
	}
	return service
}

// startIdleService has no handler, so queued jobs stay queued.
func startIdleService(t *testing.T) *Service {
	t.Helper()
	service, err := NewService(ServiceOptions{Directory: t.TempDir()})
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
	service := startIdleService(t)
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
	service := startIdleService(t)
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
