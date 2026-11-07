package task_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

var errBoom = errors.New("boom")

// syncBuffer is a log sink safe for concurrent tasks.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newRoot(t *testing.T) (*task.Group, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	return task.NewRoot(t.Context(), "root", zerolog.New(logs)), logs
}

func stopErr(group *task.Group) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return group.Stop(ctx)
}

func stop(t *testing.T, group *task.Group) {
	t.Helper()
	if err := stopErr(group); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func waitDone(t *testing.T, group *task.Group) {
	t.Helper()
	select {
	case <-group.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("group never finished")
	}
}

func block(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func TestStopCancelsAndWaits(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	var exited atomic.Bool
	if err := group.Go("wait", func(ctx context.Context) error {
		<-ctx.Done()
		exited.Store(true)
		return ctx.Err()
	}); err != nil {
		t.Fatalf("go: %v", err)
	}
	stop(t, group)
	if !exited.Load() {
		t.Fatal("Stop returned before the task exited")
	}
	if state := group.State(); state != task.Stopped {
		t.Fatalf("state = %s, want stopped", state)
	}
	if !errors.Is(group.Cause(), task.ErrStopped) || group.Err() != nil {
		t.Fatalf("cause=%v err=%v, want ErrStopped and no failure", group.Cause(), group.Err())
	}
}

func TestGoAfterStopIsRefused(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	stop(t, group)
	ran := false
	err := group.Go("late", func(context.Context) error { ran = true; return nil })
	if !errors.Is(err, task.ErrStopped) || ran {
		t.Fatalf("err=%v ran=%v, want ErrStopped and nothing run", err, ran)
	}
}

func TestChildOfStoppingOrStoppedGroupRefusesWork(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	release := make(chan struct{})
	if err := group.Go("hold", func(context.Context) error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	group.Close()
	if state := group.State(); state != task.Stopping {
		t.Fatalf("state = %s, want stopping while a task holds", state)
	}
	during := group.Child("during")
	close(release)
	waitDone(t, group)
	after := group.Child("after")
	for _, child := range []*task.Group{during, after} {
		if state := child.State(); state != task.Stopped {
			t.Fatalf("child state = %s, want stopped", state)
		}
		if err := child.Go("x", func(context.Context) error { return nil }); !errors.Is(err, task.ErrStopped) {
			t.Fatalf("child go err = %v, want ErrStopped", err)
		}
	}
}

func TestStopStopsNewestChildFirstThenOwnTasks(t *testing.T) {
	t.Parallel()
	root, _ := newRoot(t)
	var mu sync.Mutex
	var order []string
	record := func(name string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			<-ctx.Done()
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	if err := root.Go("own", record("root")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second", "third"} {
		if err := root.Child(name).Go("t", record(name)); err != nil {
			t.Fatal(err)
		}
	}
	stop(t, root)
	want := []string{"third", "second", "first", "root"}
	if !slices.Equal(order, want) {
		t.Fatalf("stop order = %v, want %v", order, want)
	}
}

// orderRecorder starts one recording task in root and in each named child.
func orderRecorder(t *testing.T, root *task.Group, names ...string) func() []string {
	t.Helper()
	var mu sync.Mutex
	var order []string
	record := func(name string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			<-ctx.Done()
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	if err := root.Go("own", record("root")); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := root.Child(name).Go("t", record(name)); err != nil {
			t.Fatal(err)
		}
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(order)
	}
}

func TestCloseStopsNewestChildFirstThenOwnTasks(t *testing.T) {
	t.Parallel()
	for range 20 {
		root, _ := newRoot(t)
		order := orderRecorder(t, root, "first", "second", "third")
		root.Close()
		waitDone(t, root)
		if got, want := order(), []string{"third", "second", "first", "root"}; !slices.Equal(got, want) {
			t.Fatalf("close order = %v, want %v", got, want)
		}
	}
}

func TestFailureStopsNewestChildFirstThenOwnTasks(t *testing.T) {
	t.Parallel()
	for range 20 {
		root, _ := newRoot(t)
		order := orderRecorder(t, root, "first", "second", "third")
		if err := root.GoCritical("serve", func(context.Context) error { return errBoom }); err != nil {
			t.Fatal(err)
		}
		waitDone(t, root)
		if got, want := order(), []string{"third", "second", "first", "root"}; !slices.Equal(got, want) {
			t.Fatalf("failure order = %v, want %v", got, want)
		}
		if root.State() != task.Failed {
			t.Fatalf("state = %s, want failed", root.State())
		}
	}
}

// TestFailingOlderChildWaitsForNewerSibling: when an older child fails, its
// newer sibling still stops first (mount before the vfs it reads from).
func TestFailingOlderChildWaitsForNewerSibling(t *testing.T) {
	t.Parallel()
	for range 20 {
		root, _ := newRoot(t)
		var mu sync.Mutex
		var order []string
		record := func(name string) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				<-ctx.Done()
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return nil
			}
		}
		if err := root.Go("own", record("root")); err != nil {
			t.Fatal(err)
		}
		vfs := root.Child("vfs")
		if err := vfs.Go("serve", record("vfs")); err != nil {
			t.Fatal(err)
		}
		if err := root.Child("mount").Go("serve", record("mount")); err != nil {
			t.Fatal(err)
		}
		if err := vfs.GoCritical("flush", func(context.Context) error { return errBoom }); err != nil {
			t.Fatal(err)
		}
		waitDone(t, root)
		mu.Lock()
		got := slices.Clone(order)
		mu.Unlock()
		if want := []string{"mount", "vfs", "root"}; !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
		if !errors.Is(root.Err(), errBoom) {
			t.Fatalf("root err = %v, want boom", root.Err())
		}
	}
}

func TestDirtyStopInGrandchildReachesRoot(t *testing.T) {
	t.Parallel()
	root, _ := newRoot(t)
	if err := root.Child("a").Child("b").GoCritical("flush", func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("final flush failed: disk full")
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, root)
	if root.State() != task.Stopped {
		t.Fatalf("state = %s, want stopped", root.State())
	}
	if err := root.Err(); err == nil || !strings.Contains(err.Error(), "disk full") ||
		!strings.Contains(err.Error(), "root/a/b") {
		t.Fatalf("root err = %v, want the grandchild's flush failure", err)
	}
}

// TestStopDeadlineReachesEveryTask: when the newest child ignores its
// context, the deadline still cancels the older siblings and g's own tasks.
func TestStopDeadlineReachesEveryTask(t *testing.T) {
	t.Parallel()
	root, _ := newRoot(t)
	olderExited := make(chan struct{})
	if err := root.Child("older").Go("w", func(ctx context.Context) error {
		<-ctx.Done()
		close(olderExited)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	if err := root.Child("newest").Go("hang", func(context.Context) error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := root.Stop(short)
	if !errors.Is(err, task.ErrStopTimeout) || !strings.Contains(err.Error(), "root/newest:hang") {
		t.Fatalf("stop err = %v, want a timeout naming root/newest:hang", err)
	}
	select {
	case <-olderExited:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung newest child kept the deadline from reaching an older sibling")
	}
	close(release)
	waitDone(t, root)
}

func TestFailedFiresBeforeShutdownEnds(t *testing.T) {
	t.Parallel()
	root, _ := newRoot(t)
	release := make(chan struct{})
	if err := root.Child("slow").Go("w", func(context.Context) error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := root.GoCritical("serve", func(context.Context) error { return errBoom }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-root.Failed():
	case <-time.After(5 * time.Second):
		t.Fatal("Failed never fired")
	}
	if !errors.Is(root.Err(), errBoom) {
		t.Fatalf("err = %v, want boom as soon as Failed fires", root.Err())
	}
	select {
	case <-root.Done():
		t.Fatal("Done closed while a child still ran")
	default:
	}
	close(release)
	waitDone(t, root)
}

func TestFailureDuringStopIsADirtyStop(t *testing.T) {
	t.Parallel()
	root, logs := newRoot(t)
	if err := root.GoCritical("flush", func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("final flush failed: disk full")
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, root)
	if root.State() != task.Stopped {
		t.Fatalf("state = %s, want stopped", root.State())
	}
	if err := root.Err(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the flush failure", err)
	}
	out := logs.String()
	if !strings.Contains(out, "Task failed during stop") || strings.Contains(out, "Task failed its group") {
		t.Fatalf("stop-time failure logged wrongly: %s", out)
	}
	select {
	case <-root.Failed():
		t.Fatal("Failed fired for a failure during a stop")
	default:
	}
}

func TestParentStoppedOnlyAfterGrandchildExits(t *testing.T) {
	t.Parallel()
	root, _ := newRoot(t)
	release := make(chan struct{})
	if err := root.Child("svc").Child("worker").Go("slow", func(context.Context) error {
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	root.Close()
	select {
	case <-root.Done():
		t.Fatal("root finished while a grandchild task still ran")
	case <-time.After(20 * time.Millisecond):
	}
	if state := root.State(); state != task.Stopping {
		t.Fatalf("root state = %s, want stopping", state)
	}
	close(release)
	waitDone(t, root)
	if state := root.State(); state != task.Stopped {
		t.Fatalf("root state = %s, want stopped", state)
	}
}

func TestStopTimeoutNamesStragglersAndParentWaitsForThem(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	release := make(chan struct{})
	child := group.Child("svc")
	if err := child.Go("stubborn", func(context.Context) error {
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := child.Stop(short); !errors.Is(err, task.ErrStopTimeout) {
		t.Fatalf("child stop err = %v, want timeout", err)
	}
	short2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	err := group.Stop(short2)
	if !errors.Is(err, task.ErrStopTimeout) || !strings.Contains(err.Error(), "root/svc:stubborn") {
		t.Fatalf("err = %v, want ErrStopTimeout naming root/svc:stubborn", err)
	}
	if state := group.State(); state != task.Stopping {
		t.Fatalf("state = %s, want stopping while a task still runs", state)
	}
	close(release)
	stop(t, group)
	if state := group.State(); state != task.Stopped {
		t.Fatalf("state = %s, want stopped", state)
	}
}

// TestStopAfterDoneNeverReportsTimeout: once Done is closed, Stop with an
// expired context must return nil, not a false timeout.
func TestStopAfterDoneNeverReportsTimeout(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	stop(t, group)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for range 1000 {
		if err := group.Stop(expired); err != nil {
			t.Fatalf("Stop after Done with an expired ctx = %v, want nil", err)
		}
	}
}

func TestCloseFromOwnTaskDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	root, _ := newRoot(t)
	svc := root.Child("svc")
	if err := svc.Go("restart", func(ctx context.Context) error {
		root.Close()
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, root)
	if state := root.State(); state != task.Stopped {
		t.Fatalf("state = %s, want stopped", state)
	}
}

func TestPanicIsRecoveredAndLogged(t *testing.T) {
	t.Parallel()
	group, logs := newRoot(t)
	done := make(chan struct{})
	if err := group.Go("bad", func(context.Context) error {
		defer close(done)
		panic("kaboom")
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := group.Go("after", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("group refused work after a non-critical panic: %v", err)
	}
	stop(t, group)
	if out := logs.String(); !strings.Contains(out, "Task panicked") || !strings.Contains(out, "kaboom") {
		t.Fatalf("panic not logged: %s", out)
	}
}

func TestFatalFailsGroup(t *testing.T) {
	t.Parallel()
	group, logs := newRoot(t)
	if err := group.Go("critical", func(context.Context) error { return task.Fatal(errBoom) }); err != nil {
		t.Fatal(err)
	}
	waitDone(t, group)
	if state := group.State(); state != task.Failed {
		t.Fatalf("state = %s, want failed", state)
	}
	if !errors.Is(group.Err(), errBoom) || !errors.Is(group.Cause(), errBoom) {
		t.Fatalf("err=%v cause=%v, want boom", group.Err(), group.Cause())
	}
	err := group.Go("next", func(context.Context) error { return nil })
	if !errors.Is(err, task.ErrStopped) || !errors.Is(err, errBoom) {
		t.Fatalf("go after failure err = %v, want ErrStopped wrapping boom", err)
	}
	stop(t, group)
	if !strings.Contains(logs.String(), "Task failed its group") {
		t.Fatalf("fatal not logged: %s", logs.String())
	}
}

// TestCriticalFailureReachesRoot: a grandchild's critical failure fails
// every ancestor, so the owner of the root sees it.
func TestCriticalFailureReachesRoot(t *testing.T) {
	t.Parallel()
	root, _ := newRoot(t)
	var sibling atomic.Bool
	if err := root.Child("other").Go("w", func(ctx context.Context) error {
		<-ctx.Done()
		sibling.Store(true)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if err := root.Child("server").Child("http").GoCritical("serve", func(context.Context) error {
		return errBoom
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-root.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a grandchild's critical failure never reached the root")
	}
	waitDone(t, root)
	if state := root.State(); state != task.Failed {
		t.Fatalf("root state = %s, want failed", state)
	}
	if err := root.Err(); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "root/server/http") {
		t.Fatalf("root err = %v, want boom naming root/server/http", err)
	}
	if !sibling.Load() {
		t.Fatal("a sibling service was not cancelled by the failure")
	}
}

func TestCriticalPanicFailsGroup(t *testing.T) {
	t.Parallel()
	group, logs := newRoot(t)
	if err := group.GoCritical("serve", func(context.Context) error { panic("kaboom") }); err != nil {
		t.Fatal(err)
	}
	waitDone(t, group)
	if err := group.Err(); err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("err = %v, want the panic", err)
	}
	if !strings.Contains(logs.String(), "Task panicked") {
		t.Fatalf("panic stack not logged: %s", logs.String())
	}
}

func TestCriticalReturningFatalIsNotWrappedTwice(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	if err := group.GoCritical("serve", func(context.Context) error { return task.Fatal(errBoom) }); err != nil {
		t.Fatal(err)
	}
	waitDone(t, group)
	if !errors.Is(group.Cause(), errBoom) || group.Cause().Error() != errBoom.Error() {
		t.Fatalf("cause = %v, want boom itself", group.Cause())
	}
}

func TestCriticalCleanExitAndStopAreNotFailures(t *testing.T) {
	t.Parallel()
	group, logs := newRoot(t)
	if err := group.GoCritical("startup", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(group.Snapshot()) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(group.Snapshot()) > 0 {
		t.Fatal("startup task never finished")
	}
	if group.Context().Err() != nil {
		t.Fatalf("a clean critical exit failed the group: %v", group.Cause())
	}
	if err := group.GoCritical("serve", block); err != nil {
		t.Fatal(err)
	}
	stop(t, group)
	if group.State() != task.Stopped || group.Err() != nil {
		t.Fatalf("state=%s err=%v, want a clean stop", group.State(), group.Err())
	}
	if strings.Contains(logs.String(), "failed") {
		t.Fatalf("stop logged as a failure: %s", logs.String())
	}
}

func TestFatalNilIsNil(t *testing.T) {
	t.Parallel()
	if err := task.Fatal(nil); err != nil {
		t.Fatalf("Fatal(nil) = %v, want nil", err)
	}
}

func TestErrorsLoggedButStopNoiseIsNot(t *testing.T) {
	t.Parallel()
	group, logs := newRoot(t)
	done := make(chan struct{})
	if err := group.Go("flaky", func(context.Context) error { defer close(done); return errBoom }); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := group.Go("quiet", func(ctx context.Context) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Go("flush", func(ctx context.Context) error {
		<-ctx.Done()
		return errors.Join(errors.New("final flush lost 12 records"), ctx.Err())
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, group)
	out := logs.String()
	if !strings.Contains(out, "root:flaky") || !strings.Contains(out, "root:flush") {
		t.Fatalf("real errors not logged: %s", out)
	}
	if strings.Contains(out, "root:quiet") {
		t.Fatalf("stop cancellation logged as a failure: %s", out)
	}
}

func TestCancelledWhileLiveIsLogged(t *testing.T) {
	t.Parallel()
	group, logs := newRoot(t)
	done := make(chan struct{})
	if err := group.Go("odd", func(context.Context) error { defer close(done); return context.Canceled }); err != nil {
		t.Fatal(err)
	}
	<-done
	stop(t, group)
	if !strings.Contains(logs.String(), "root:odd") {
		t.Fatalf("a cancellation unrelated to the group was hidden: %s", logs.String())
	}
}

func TestSnapshotListsRunningTasks(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	if err := group.Go("a", block); err != nil {
		t.Fatal(err)
	}
	if err := group.Child("svc").Go("b", block); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, info := range group.Snapshot() {
		paths = append(paths, info.Path)
		if info.Started.IsZero() {
			t.Errorf("%s has no start time", info.Path)
		}
	}
	if want := []string{"root/svc:b", "root:a"}; !slices.Equal(paths, want) {
		t.Fatalf("snapshot = %v, want %v", paths, want)
	}
	stop(t, group)
	if left := group.Snapshot(); len(left) != 0 {
		t.Fatalf("snapshot after stop = %v, want empty", left)
	}
}

func TestStopIsSafeConcurrently(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	if err := group.Go("w", block); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- stopErr(group) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent stop: %v", err)
		}
	}
	if state := group.State(); state != task.Stopped {
		t.Fatalf("state = %s, want stopped", state)
	}
}

// TestGoRacingStopNeverLosesATask: Stop starts while Go calls are in
// flight; every task Go accepted runs before the group reports Stopped, and
// a refused one never runs.
func TestGoRacingStopNeverLosesATask(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	var accepted, ran, late atomic.Int64
	started := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 50 {
				err := group.Go("n", func(context.Context) error {
					if group.State() == task.Stopped {
						late.Add(1)
					}
					ran.Add(1)
					return nil
				})
				if err == nil {
					accepted.Add(1)
				}
				once.Do(func() { close(started) })
			}
		})
	}
	<-started
	stopResult := stopErr(group)
	stoppedRan := ran.Load()
	wg.Wait()
	if stopResult != nil {
		t.Fatalf("stop: %v", stopResult)
	}
	if late.Load() != 0 {
		t.Fatalf("%d tasks ran after the group reported stopped", late.Load())
	}
	if accepted.Load() != stoppedRan || ran.Load() != stoppedRan {
		t.Fatalf("accepted %d, ran %d by Stop's return, %d in total", accepted.Load(), stoppedRan, ran.Load())
	}
}

func TestParentCancellationReachesTasks(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	group := task.NewRoot(parent, "root", zerolog.Nop())
	exited := make(chan struct{})
	if err := group.Child("svc").Go("w", func(ctx context.Context) error {
		<-ctx.Done()
		close(exited)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancellation did not reach the child's task")
	}
	if err := group.Go("late", func(context.Context) error { return nil }); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("go on a cancelled group err = %v, want ErrStopped", err)
	}
	stop(t, group)
}

func TestDetachedKeepsValuesDropsCancel(t *testing.T) {
	t.Parallel()
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
	cancel()
	ctx, release := task.Detached(parent, time.Hour)
	defer release()
	if ctx.Err() != nil {
		t.Fatalf("detached context inherited cancellation: %v", ctx.Err())
	}
	if ctx.Value(key{}) != "v" {
		t.Fatal("detached context lost its values")
	}
	short, releaseShort := task.Detached(parent, time.Millisecond)
	defer releaseShort()
	<-short.Done()
	if !errors.Is(short.Err(), context.DeadlineExceeded) {
		t.Fatalf("detached timeout err = %v, want deadline exceeded", short.Err())
	}
}
