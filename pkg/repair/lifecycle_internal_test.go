package repair

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// testGroup returns a running task group, closed and awaited when the test ends.
func testGroup(t testing.TB) *task.Group {
	t.Helper()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		g.Close()
		<-g.Done()
	})
	return g
}

// exitWatch receives the ID of every run or recheck task that has released its slot and returned.
type exitWatch chan string

// watchExits makes s report each finished run or recheck, so a test waits for the task without polling.
func (r *Service) watchExits() exitWatch {
	ch := make(exitWatch, 64)
	r.onRunExit = func(id string) { ch <- id }
	return ch
}

// wait blocks until one more run or recheck has finished.
func (w exitWatch) wait() { <-w }

// holdChecks makes every probe check announce itself on entered and wait for release, ignoring cancellation, as a
// sweep step stuck in a provider call would. Every check after release passes at once.
func (f *outageFixture) holdChecks(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	entered := make(chan struct{}, 16)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	f.client.mu.Lock()
	f.client.onCheck = func(int) {
		entered <- struct{}{}
		<-gate
	}
	f.client.mu.Unlock()
	return entered, release
}

// sweepTasks counts the sweep tasks the service's group lists.
func sweepTasks(g *task.Group) int {
	n := 0
	for _, info := range g.Snapshot() {
		if strings.Contains(info.Path, "sweep ") {
			n++
		}
	}
	return n
}

func repairRunCount(t *testing.T, f *outageFixture) int {
	t.Helper()
	runs, err := f.store.ListRepairRuns()
	if err != nil {
		t.Fatal(err)
	}
	return len(runs)
}

func TestRunNowRacingStopNeverTwoSweeps(t *testing.T) {
	t.Parallel()
	t.Run("stop first", func(t *testing.T) {
		t.Parallel()
		f := newOutageFixture(t, 1, config.RepairConfig{})
		f.service.Stop()
		if _, err := f.service.RunNow(RunOptions{}); !errors.Is(err, task.ErrStopped) {
			t.Fatalf("RunNow after Stop = %v, want task.ErrStopped", err)
		}
		if n := repairRunCount(t, f); n != 0 {
			t.Fatalf("%d run records after a refused RunNow, want none", n)
		}
	})
	t.Run("run first", func(t *testing.T) {
		t.Parallel()
		runNowAfterStopWithLiveSweep(t)
	})
	t.Run("concurrent", func(t *testing.T) {
		t.Parallel()
		runNowConcurrentWithStop(t)
	})
}

func runNowAfterStopWithLiveSweep(t *testing.T) {
	t.Helper()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	entered, release := f.holdChecks(t)
	if _, err := f.service.RunNow(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	f.service.Stop()
	if _, err := f.service.RunNow(RunOptions{}); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("RunNow after Stop with a sweep live = %v, want task.ErrStopped", err)
	}
	if n := sweepTasks(f.g); n != 1 {
		t.Fatalf("%d sweep tasks listed, want exactly the live one", n)
	}
	release()
	f.exits.wait()
	if n := repairRunCount(t, f); n != 1 {
		t.Fatalf("%d run records, want only the first sweep's", n)
	}
}

func runNowConcurrentWithStop(t *testing.T) {
	t.Helper()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	_, release := f.holdChecks(t)
	var wg sync.WaitGroup
	var started atomic.Int32
	for range 8 {
		wg.Go(func() {
			if _, err := f.service.RunNow(RunOptions{}); err == nil {
				started.Add(1)
			}
		})
	}
	wg.Go(f.service.Stop)
	wg.Wait()
	if n := sweepTasks(f.g); n > 1 {
		t.Fatalf("%d sweep tasks listed at once, want at most 1", n)
	}
	release()
	f.g.Close()
	<-f.g.Done()
	if got := started.Load(); got > 1 {
		t.Fatalf("%d RunNow calls started a sweep, want at most 1", got)
	}
	if n := repairRunCount(t, f); n > 1 {
		t.Fatalf("%d run records, want at most 1", n)
	}
}

func TestApplyConfigDuringRunRefusesRunNowUntilTaskExits(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	entered, release := f.holdChecks(t)
	if _, err := f.service.RunNow(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := f.service.ApplyConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RunNow(RunOptions{}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("RunNow while the cancelled sweep's task lives = %v, want already running", err)
	}
	if _, err := f.service.ClearStates([]storage.HealthStatus{storage.HealthBroken}); err == nil {
		t.Fatal("ClearStates was accepted while the cancelled sweep's task lives")
	}
	release()
	f.exits.wait()
	if _, err := f.service.RunNow(RunOptions{}); err != nil {
		t.Fatalf("RunNow after the task exited = %v, want accepted", err)
	}
	f.exits.wait()
}

func TestApplyConfigKeepsRunRecordRunning(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	entered, release := f.holdChecks(t)
	id, err := f.service.RunNow(RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err = f.service.ApplyConfig(); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.GetRepairRun(id)
	if err != nil || run == nil {
		t.Fatalf("run record = %v, %v", run, err)
	}
	if run.Status != storage.RepairRunRunning {
		t.Fatalf("run record is %q after ApplyConfig, want running until its task ends", run.Status)
	}
	release()
	f.exits.wait()
	if run, err = f.store.GetRepairRun(id); err != nil || run.Status == storage.RepairRunRunning {
		t.Fatalf("run record = %+v, %v after the task ended, want a final status", run, err)
	}
}

func TestReconcileOrphansSkipsOwnedRun(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	entered, release := f.holdChecks(t)
	id, err := f.service.RunNow(RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err = f.service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.GetRepairRun(id)
	if err != nil || run == nil || run.Status != storage.RepairRunRunning {
		t.Fatalf("owned run after Start = %+v, %v, want still running", run, err)
	}
	release()
	f.exits.wait()
}

func TestRunNowAfterStopRefused(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	f.service.Stop()
	if _, err := f.service.RunNow(RunOptions{}); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("RunNow after Stop = %v, want task.ErrStopped", err)
	}
}

func TestRecheckAfterStopRefused(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	f.service.Stop()
	if _, err := f.service.RecheckEntry(
		t.Context(),
		f.entries[0].Name,
		RecheckOptions{},
	); !errors.Is(
		err,
		task.ErrStopped,
	) {
		t.Fatalf("RecheckEntry after Stop = %v, want task.ErrStopped", err)
	}
}

func TestRefusedRecheckReturnsStopped(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	f.g.Close()
	<-f.g.Done()
	h, err := f.service.RecheckEntry(t.Context(), f.entries[0].Name, RecheckOptions{})
	if !errors.Is(err, task.ErrStopped) || h != nil {
		t.Fatalf("RecheckEntry on a stopped group = %+v, %v, want task.ErrStopped and no state", h, err)
	}
}

func TestRefusedGoReleasesSlotWritesNothing(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	f.g.Close()
	<-f.g.Done()
	if _, err := f.service.RunNow(RunOptions{}); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("RunNow on a stopped group = %v, want task.ErrStopped", err)
	}
	if _, err := f.service.RecheckMedia(
		t.Context(),
		"radarr",
		"1",
		RecheckOptions{},
	); !errors.Is(
		err,
		task.ErrStopped,
	) {
		t.Fatalf("RecheckMedia on a stopped group = %v, want task.ErrStopped", err)
	}
	if n := repairRunCount(t, f); n != 0 {
		t.Fatalf("%d run records after refused runs, want none", n)
	}
	f.service.mu.Lock()
	held := f.service.activeRunID
	f.service.mu.Unlock()
	if held != "" {
		t.Fatalf("run slot still held by %q after a refusal", held)
	}
}

func TestApplyConfigReturnsWithoutWaiting(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	entered, release := f.holdChecks(t)
	if _, err := f.service.RunNow(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := f.service.ApplyConfig(); err != nil {
		t.Fatal(err)
	}
	if n := sweepTasks(f.g); n != 1 {
		t.Fatalf("%d sweep tasks listed after ApplyConfig returned, want the cancelled one still there", n)
	}
	release()
	f.exits.wait()
}

// ApplyConfig only cancels: a probe that ignores its cancellation does not hold it. The sweep fixture serves real
// sockets, which keep a synctest clock from advancing, so a stuck probe stands in for the stuck sweep.
func TestApplyConfigDoesNotWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		service := New(testGroup(t), Dependencies{})
		block := make(chan struct{})
		release := sync.OnceFunc(func() { close(block) })
		defer release() // a failed assertion must not leave the probe blocked, or synctest hides it behind a deadlock
		started := make(chan struct{})
		service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
			close(started)
			<-block // a probe that ignores its cancellation
			return canaryInconclusive
		}
		go service.QueueCanary(t.Context(), "rd")
		<-started

		begin := time.Now()
		if err := service.ApplyConfig(); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(begin); waited != 0 {
			t.Fatalf("ApplyConfig waited %s with a stuck probe, want no wait", waited)
		}
		release()
		synctest.Wait()
	})
}

// repairJobs counts the repair-tagged jobs registered on s.
func repairJobs(s gocron.Scheduler) int {
	n := 0
	for _, job := range s.Jobs() {
		if slices.Contains(job.Tags(), repairSchedulerTag) || slices.Contains(job.Tags(), repairStopSchedulerTag) {
			n++
		}
	}
	return n
}

func newScheduledService(t *testing.T) (*Service, gocron.Scheduler) {
	t.Helper()
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Shutdown() })
	service := New(testGroup(t), Dependencies{
		Scheduler: scheduler,
		Storage:   newRepairTestStorage(t),
		Config: config.NewStore(&config.Config{Repair: config.RepairConfig{
			Enabled: true, Schedule: "24h", StopSchedule: "25h",
		}}),
	})
	return service, scheduler
}

func TestApplyConfigAfterStopRegistersNoJobs(t *testing.T) {
	t.Parallel()
	service, scheduler := newScheduledService(t)
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := repairJobs(scheduler); n != 2 {
		t.Fatalf("%d repair jobs after Start, want 2", n)
	}
	service.Stop()
	if n := repairJobs(scheduler); n != 0 {
		t.Fatalf("%d repair jobs after Stop, want none", n)
	}
	if err := service.ApplyConfig(); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("ApplyConfig after Stop = %v, want task.ErrStopped", err)
	}
	if n := repairJobs(scheduler); n != 0 {
		t.Fatalf("%d repair jobs after a refused ApplyConfig, want none", n)
	}
}

func TestStopAfterApplyConfigRemovesJobs(t *testing.T) {
	t.Parallel()
	service, scheduler := newScheduledService(t)
	if err := service.ApplyConfig(); err != nil {
		t.Fatal(err)
	}
	if n := repairJobs(scheduler); n != 2 {
		t.Fatalf("%d repair jobs after ApplyConfig, want 2", n)
	}
	service.Stop()
	if n := repairJobs(scheduler); n != 0 {
		t.Fatalf("%d repair jobs after Stop, want none", n)
	}
}

func TestApplyConfigRacingStop(t *testing.T) {
	t.Parallel()
	for range 16 {
		service, scheduler := newScheduledService(t)
		if err := service.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Go(service.Stop)
		wg.Go(func() { _ = service.ApplyConfig() })
		wg.Wait()
		if n := repairJobs(scheduler); n != 0 {
			t.Fatalf("%d repair jobs registered after Stop returned, want none", n)
		}
	}
}

func TestRushedStopCancelsFinalHeal(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "a.mkv", "b.mkv")
	f.probe(t.Context(), t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.reacquirer.reacquire = func(reacquire.Request) (*reacquire.Job, error) {
		if len(f.reacquirer.requests) == 1 {
			cancel()
			return &reacquire.Job{ID: "job"}, nil
		}
		return nil, reacquire.ErrBindingNotFound
	}
	batch := f.probe(ctx, t, true)
	healing := make(chan struct{})
	healCtxEnded := make(chan bool, 1)
	f.reacquirer.libraryWith = func(healCtx context.Context, _ reacquire.LibraryRequest) (*reacquire.Job, error) {
		close(healing)
		select {
		case <-healCtx.Done():
			healCtxEnded <- true
		case <-time.After(5 * time.Second): // watchdog: only reached when the heal is not bound to the group
			healCtxEnded <- false
		}
		return nil, healCtx.Err()
	}
	stop := &repairStopState{}
	stop.set()
	finished := make(chan struct{})
	if err := f.service.g.Go("sweep final", func(context.Context) error {
		defer close(finished)
		f.service.finishCancelledRepairSweep(f.run, stop, true, "stopped", batch)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-healing
	rushed, expire := context.WithCancel(context.Background())
	expire()
	_ = f.service.g.Stop(rushed)
	<-finished
	if !<-healCtxEnded {
		t.Fatal("the stop-schedule final heal kept running after the rushed group stop")
	}
}

func TestStopCancelsRecheckWithinDeadline(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	f.client.mu.Lock()
	f.client.blockCheck = true
	f.client.entered = make(chan struct{}, 4)
	entered := f.client.entered
	f.client.mu.Unlock()
	if _, err := f.service.RecheckEntry(
		context.WithoutCancel(t.Context()),
		f.entries[0].Name,
		RecheckOptions{},
	); err != nil {
		t.Fatal(err)
	}
	<-entered
	f.g.Close()
	<-f.g.Done()
	select {
	case <-f.exits:
	default:
		t.Fatal("the group reported done while the recheck was still running")
	}
}

func TestManualRunDoesNotContinueAfterGroupStopped(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	running := make(chan struct{})
	var finished atomic.Bool
	_, err := f.service.startManualRun(
		context.WithoutCancel(t.Context()),
		newManualRun(storage.RepairStageSelecting, "test"),
		func(ctx context.Context) {
			close(running)
			<-ctx.Done()
			finished.Store(true)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	<-running
	f.g.Close()
	<-f.g.Done()
	if !finished.Load() {
		t.Fatal("the group reported done while the manual run's work was still going")
	}
}

func TestSnapshotEmptyAfterStop(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	if _, err := f.service.RunNow(RunOptions{}); err != nil {
		t.Fatal(err)
	}
	f.service.Stop()
	f.g.Close()
	<-f.g.Done()
	if left := f.g.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %+v", left)
	}
}

func TestCancelledRecheckSavesNoRollup(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	f.client.mu.Lock()
	f.client.blockCheck = true
	f.client.entered = make(chan struct{}, 4)
	entered := f.client.entered
	f.client.mu.Unlock()
	if _, err := f.service.RecheckEntry(
		context.WithoutCancel(t.Context()),
		f.entries[0].Name,
		RecheckOptions{},
	); err != nil {
		t.Fatal(err)
	}
	<-entered
	f.g.Close()
	<-f.g.Done()
	h, err := f.store.GetEntryHealth(f.entries[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if !h.LastCheckedAt.IsZero() || h.ActiveRunID != "" {
		t.Fatalf("health = %+v, want no rollup of the cancelled probe and no active run", h)
	}
}
