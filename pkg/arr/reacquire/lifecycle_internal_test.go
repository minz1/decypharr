package reacquire

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

// deadlockWatchdog is how long a lifecycle deadlock test waits in real time. RWMutex deadlocks are invisible to
// testing/synctest, so these tests run outside a bubble and fail on this wall-clock limit instead.
const deadlockWatchdog = 5 * time.Second

// within runs fn on its own goroutine and fails the test if it has not returned after the watchdog. fn reports with
// t.Error, never t.Fatal, because it is not the test goroutine.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(deadlockWatchdog):
		t.Fatalf("%s deadlocked", what)
	}
}

// rushStop stops g with an already-expired context, as the root does at its deadline.
func rushStop(g *task.Group) error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return g.Stop(ctx)
}

func testBinding() Binding {
	return Binding{
		ArrName:                "radarr",
		ArrType:                arr.Radarr,
		ArrInstanceFingerprint: testArrInstanceFingerprint,
		EntryID:                "broken-entry",
		EntryFileID:            "broken-file",
		DownloadID:             "broken-download",
		ArrFileID:              40,
		LibraryPath:            "/library/movie.mkv",
		MovieID:                7,
		Confidence:             ConfidenceExactPath,
	}
}

func replacementBinding() Binding {
	replacement := testBinding()
	replacement.EntryID = "replacement-entry"
	replacement.EntryFileID = "replacement-file"
	replacement.DownloadID = "replacement-download"
	replacement.ArrFileID = 41
	return replacement
}

// storeWaitingJob persists, before Start, a job that waits for a replacement of its download.
// With indexed set the replacement binding is stored too, so Start finds it already indexed.
func storeWaitingJob(t *testing.T, service *Service, indexed bool) {
	t.Helper()
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	broken := testBinding()
	job := Job{
		ID:         "waiting",
		Status:     StatusWaitingForImport,
		Cause:      CauseManual,
		Strategy:   StrategyHistoryFailed,
		ArrName:    broken.ArrName,
		ArrType:    broken.ArrType,
		EntryID:    broken.EntryID,
		FileID:     broken.EntryFileID,
		DownloadID: broken.DownloadID,
		Bindings:   []Binding{broken},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := service.jobRepository.Save(job); err != nil {
		t.Fatal(err)
	}
	if indexed {
		replacement := replacementBinding()
		replacement.UpdatedAt = now
		if err := service.bindingRepository.Save(replacement); err != nil {
			t.Fatal(err)
		}
	}
}

func newTestService(t *testing.T, g *task.Group, directory string) *Service {
	t.Helper()
	service, err := NewService(g, ServiceOptions{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func stopGroup(g *task.Group) {
	g.Close()
	<-g.Done()
}

func TestEnqueueBeforeStartNotStarted(t *testing.T) {
	t.Parallel()
	// Regression test (passes on the old code): a request before Start is refused, not queued.
	service := newTestService(t, newTestGroup(t), t.TempDir())
	_, err := service.Reacquire(Request{EntryID: "e", FileID: "f", Cause: CauseStream})
	if !errors.Is(err, ErrServiceNotStarted) {
		t.Fatalf("Reacquire before Start = %v, want %v", err, ErrServiceNotStarted)
	}
}

func TestRequestBetweenStartAndLoopIsQueued(t *testing.T) {
	t.Parallel()
	// Regression test (passes on the old code): a request right after Start returns is queued and runs once a
	// handler is set, whether or not the loop has reached its first receive.
	handler := &waitingReacquireHandler{called: make(chan string, 1)}
	service := newTestService(t, newTestGroup(t), t.TempDir())
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.UpsertBinding(testBinding()); err != nil {
		t.Fatal(err)
	}
	job, err := service.Reacquire(Request{EntryID: "broken-entry", FileID: "broken-file", Cause: CauseStream})
	if err != nil {
		t.Fatalf("Reacquire right after Start = %v", err)
	}
	if err = service.SetHandler(handler); err != nil {
		t.Fatal(err)
	}
	if got := <-handler.called; got != job.ID {
		t.Fatalf("handler ran job %q, want %q", got, job.ID)
	}
}

func TestStartCompletesWaitingJobAlreadyIndexed(t *testing.T) {
	t.Parallel()
	service := newTestService(t, newTestGroup(t), t.TempDir())
	storeWaitingJob(t, service, true)
	within(t, "Start", func() {
		if err := service.Start(t.Context()); err != nil {
			t.Errorf("Start = %v", err)
		}
	})
	job, ok := service.Job("waiting")
	if !ok || job.Status != StatusReady || job.ReplacementDownloadID != "replacement-download" {
		t.Fatalf("job after Start = %#v, found = %v", job, ok)
	}
}

func TestUpsertCompletingJobWhileClosing(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	service := newTestService(t, g, t.TempDir())
	storeWaitingJob(t, service, false)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	service.beforeMaintenanceCtx = func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	within(t, "UpsertBinding racing Close", func() {
		upserted := make(chan error, 1)
		go func() { upserted <- service.UpsertBinding(replacementBinding()) }()
		<-entered
		closed := make(chan struct{})
		go func() {
			stopGroup(g)
			close(closed)
		}()
		// A writer waiting for the lock is the only thing that makes TryRLock fail here.
		for service.lifecycleMu.TryRLock() {
			service.lifecycleMu.RUnlock()
			runtime.Gosched()
		}
		close(release)
		if err := <-upserted; err != nil && !errors.Is(err, ErrServiceClosed) {
			t.Errorf("UpsertBinding = %v", err)
		}
		<-closed
	})
}

func TestServiceCloseRushWithWriteInFlight(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	g := newTestGroup(t)
	service := newTestService(t, g, directory)
	storeWaitingJob(t, service, false)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	service.beforeMaintenanceCtx = func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	upserted := make(chan error, 1)
	go func() { upserted <- service.UpsertBinding(replacementBinding()) }()
	<-entered

	// The binding is saved and the operation is in flight when the stop is rushed.
	_ = rushStop(g)
	select {
	case <-g.Done():
		t.Error("the group finished while a write was in flight: the repositories could be closed under it")
	default:
	}
	close(release)
	within(t, "rushed stop", func() {
		if err := <-upserted; err != nil && !errors.Is(err, ErrServiceClosed) {
			t.Errorf("UpsertBinding = %v", err)
		}
		<-g.Done()
	})

	reopened, err := NewService(newTestGroup(t), ServiceOptions{Directory: directory})
	if err != nil {
		t.Fatalf("reopen after rushed stop: %v", err)
	}
	if err = reopened.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Lookup("replacement-entry", "replacement-file"); !ok {
		t.Error("the write that returned nil is not on disk")
	}
}

func TestStopWithoutStartClosesRepositories(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	g := newTestGroup(t)
	if _, err := NewService(g, ServiceOptions{Directory: directory}); err != nil {
		t.Fatal(err)
	}
	stopGroup(g)
	reopened, err := NewService(newTestGroup(t), ServiceOptions{Directory: directory})
	if err != nil {
		t.Fatalf("reopen after a stop without Start: %v", err)
	}
	if err = reopened.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestStartLoadErrorFails(t *testing.T) {
	t.Parallel()
	service := newTestService(t, newTestGroup(t), t.TempDir())
	if err := service.bindingRepository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(t.Context()); err == nil {
		t.Fatal("Start loaded from a closed repository")
	}
	if err := service.UpsertBinding(testBinding()); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("UpsertBinding after a failed Start = %v, want %v", err, ErrServiceClosed)
	}
	if service.Ready() {
		t.Fatal("a service whose Start failed reports ready")
	}
}

func TestOperationsAfterStopClosed(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	service := newTestService(t, g, t.TempDir())
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !service.Ready() {
		t.Fatal("a started service is not ready")
	}
	stopGroup(g)
	operations := map[string]func() error{
		"UpsertBinding": func() error { return service.UpsertBinding(testBinding()) },
		"DeleteBinding": func() error { return service.DeleteBinding("e", "f") },
		"ReplaceArrGeneration": func() error {
			return service.ReplaceArrGeneration("radarr", 1, nil)
		},
		"Reacquire": func() error {
			_, err := service.Reacquire(Request{EntryID: "e", FileID: "f", Cause: CauseStream})
			return err
		},
		"FailDownload": func() error {
			_, err := service.FailDownload("radarr", "d", "e")
			return err
		},
		"DeleteJobs": func() error {
			_, err := service.DeleteJobs([]string{"x"})
			return err
		},
		"SetHandler": func() error { return service.SetHandler(nil) },
		"Start":      func() error { return service.Start(t.Context()) },
	}
	for name, operation := range operations {
		if err := operation(); !errors.Is(err, ErrServiceClosed) {
			t.Errorf("%s after stop = %v, want %v", name, err, ErrServiceClosed)
		}
	}
	if service.Ready() {
		t.Error("a stopped service reports ready")
	}
}

func TestServiceSnapshotEmptyAfterStop(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	service := newTestService(t, g, t.TempDir())
	// A job that retries in an hour leaves a retry timer pending.
	now := time.Now().UTC()
	broken := testBinding()
	if err := service.jobRepository.Save(Job{
		ID: "retrying", Status: StatusQueued, Cause: CauseManual, Strategy: StrategyHistoryFailed,
		ArrName: broken.ArrName, ArrType: broken.ArrType, EntryID: broken.EntryID, FileID: broken.EntryFileID,
		Bindings: []Binding{broken}, CreatedAt: now, UpdatedAt: now, RetryAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, info := range g.Snapshot() {
		paths = append(paths, info.Path)
	}
	for _, want := range []string{"/worker:retry", "/worker:run", ":close"} {
		if !slices.ContainsFunc(paths, func(path string) bool { return strings.HasSuffix(path, want) }) {
			t.Errorf("snapshot while running lacks %q: %v", want, paths)
		}
	}
	stopGroup(g)
	if left := g.Snapshot(); len(left) != 0 {
		t.Fatalf("snapshot after stop = %v", left)
	}
}

// newTestGroup is the group a test service runs on; it is stopped when the test ends.
func newTestGroup(t *testing.T) *task.Group {
	t.Helper()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		g.Close()
		<-g.Done()
	})
	return g
}
