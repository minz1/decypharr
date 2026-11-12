package dfs

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend"
	fuseconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs"
)

// callLog records the order (and fake time) of the fakes' calls.
type callLog struct {
	mu      sync.Mutex
	entries []callEntry
}

type callEntry struct {
	name string
	at   time.Time
}

func (c *callLog) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, callEntry{name, time.Now()})
}

func (c *callLog) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, len(c.entries))
	for i, e := range c.entries {
		names[i] = e.name
	}
	return names
}

func (c *callLog) at(name string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if e.name == name {
			return e.at
		}
	}
	return time.Time{}
}

// syncBuffer is a log sink the owner and test goroutines share.
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

// fakeHandle is a live mount whose Unmount and Detach are the test's.
type fakeHandle struct {
	calls *callLog
	done  chan struct{}
	once  sync.Once

	unmountErr error
	// hang, when set, blocks Unmount until it is closed.
	hang chan struct{}
	// detachReleases makes Detach close hang, like a lazy detach freeing the
	// server's unmount.
	detachReleases bool
	hangOnce       sync.Once
	// detachDeadline is the unix nanoseconds of the last Detach ctx deadline; 0 when it had none.
	detachDeadline atomic.Int64
	// detachCtxDone records that the last Detach got a ctx that had already ended.
	detachCtxDone atomic.Bool
}

func newFakeHandle(calls *callLog) *fakeHandle {
	return &fakeHandle{calls: calls, done: make(chan struct{})}
}

func (h *fakeHandle) end() { h.once.Do(func() { close(h.done) }) }

func (h *fakeHandle) release() {
	if h.hang != nil {
		h.hangOnce.Do(func() { close(h.hang) })
	}
}

func (h *fakeHandle) Done() <-chan struct{} { return h.done }

// Unmount ends the serve loop synchronously, like the real server's does.
func (h *fakeHandle) Unmount() error {
	h.calls.add("unmount")
	if h.hang != nil {
		<-h.hang
	}
	if h.unmountErr != nil {
		return h.unmountErr
	}
	h.end()
	return nil
}

func (h *fakeHandle) Detach(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		h.detachDeadline.Store(deadline.UnixNano())
	}
	h.detachCtxDone.Store(ctx.Err() != nil)
	h.calls.add("detach")
	h.end()
	if h.detachReleases {
		h.release()
	}
	return nil
}

type fakeBackend struct {
	calls  *callLog
	handle *fakeHandle
	gate   chan struct{} // Mount blocks until closed
	// untilCancelled makes Mount block until its context ends, then fail with
	// the context error, like cgofuse waiting for Init.
	untilCancelled bool
	mountErr       error
	mounts         int
	// onMount runs in Mount, before it returns.
	onMount func(g *task.Group)
	mu      sync.Mutex
}

func (b *fakeBackend) Mount(ctx context.Context, g *task.Group) (backend.Mounted, error) {
	b.mu.Lock()
	b.mounts++
	b.mu.Unlock()
	if b.untilCancelled {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if b.gate != nil {
		<-b.gate
	}
	if b.onMount != nil {
		b.onMount(g)
	}
	if b.mountErr != nil {
		return nil, b.mountErr
	}
	b.calls.add("mount")
	return b.handle, nil
}

func (b *fakeBackend) mountCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mounts
}

func (b *fakeBackend) Refresh(string) {}

func (b *fakeBackend) Type() backend.Type { return backend.Hanwen }

// lifecycleFixture is a manager on a root group with fakes, built inside a
// synctest bubble.
type lifecycleFixture struct {
	m      *Manager
	root   *task.Group
	parent *task.Group
	fb     *fakeBackend
	calls  *callLog
	logs   *syncBuffer
}

func newFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	calls := &callLog{}
	logs := &syncBuffer{}
	fb := &fakeBackend{calls: calls, handle: newFakeHandle(calls)}
	cfg := fuseconfig.DefaultFuseConfig()
	cfg.CacheDir = t.TempDir()
	cfg.MountPath = t.TempDir()
	cfg.CacheCleanupInterval = time.Hour
	cfg.DaemonTimeout = 10 * time.Second
	m := &Manager{
		logger:             zerolog.New(logs).Level(zerolog.DebugLevel),
		state:              state.NewAtomic(dfsNew, dfsSuccessors),
		defaultBackendType: backend.Hanwen,
		config:             cfg,
		logs:               logger.Discard(),
	}
	m.newBackend = func(*vfs.Manager) (backend.Backend, error) { return fb, nil }
	m.closeVFS = func() error {
		calls.add("vfsClose")
		return m.vfs.Close()
	}
	root := task.NewRoot(t.Context(), "root", zerolog.Nop())
	return &lifecycleFixture{m: m, root: root, parent: root.Child("mount"), fb: fb, calls: calls, logs: logs}
}

// stopParent stops the mount service group the way the generation does.
func (f *lifecycleFixture) stopParent(t *testing.T, budget time.Duration) error {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), budget)
	defer cancel()
	return f.parent.Stop(ctx)
}

// finish stops everything so the bubble ends clean.
func (f *lifecycleFixture) finish(t *testing.T) {
	t.Helper()
	f.fb.handle.release()
	ctx, cancel := task.Detached(t.Context(), time.Minute)
	defer cancel()
	_ = f.root.Stop(ctx)
}

func (f *lifecycleFixture) rootFailed() bool {
	select {
	case <-f.root.Failed():
		return true
	default:
		return false
	}
}

// startAsync runs Start in its own goroutine, as the generation does.
func (f *lifecycleFixture) startAsync() <-chan error {
	res := make(chan error, 1)
	go func() { res <- f.m.Start(f.parent) }()
	return res
}

func requireCalls(t *testing.T, f *lifecycleFixture, want ...string) {
	t.Helper()
	if got := f.calls.names(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func requireState(t *testing.T, m *Manager, want dfsState) {
	t.Helper()
	if got := m.state.Load(); got != want {
		t.Fatalf("state = %v, want %v", got, want)
	}
}

func TestStopEndsStoppedReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		requireState(t, f.m, dfsReady)
		if !f.m.IsReady() {
			t.Fatal("not ready after Start")
		}
		if err := f.stopParent(t, 30*time.Second); err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		requireStopped(t, f.m)
	})
}

func TestStopEndsStoppedAfterDeath(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		f.fb.handle.end()
		synctest.Wait()
		requireState(t, f.m, dfsFailed)
		requireStopped(t, f.m)
	})
}

func TestStopEndsStoppedAfterStartFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.mountErr = errors.New("boom")
		if err := f.m.Start(f.parent); err == nil {
			t.Fatal("Start succeeded")
		}
		synctest.Wait()
		requireState(t, f.m, dfsFailed)
		requireCalls(t, f, "vfsClose")
		requireStopped(t, f.m)
	})
}

// requireStopped runs Manager.Stop and wants Stopped, not ready.
func requireStopped(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	requireState(t, m, dfsStopped)
	if m.IsReady() {
		t.Fatal("ready after stop")
	}
}

func TestStartTwice(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		if err := f.m.Start(f.parent); !errors.Is(err, errStarted) {
			t.Fatalf("second Start = %v, want errStarted", err)
		}
		if got := f.fb.mountCount(); got != 1 {
			t.Fatalf("mounts = %d, want 1", got)
		}
	})
}

func TestStopBeforeStart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Stop(); err != nil {
			t.Fatalf("Stop = %v", err)
		}
		requireState(t, f.m, dfsStopped)
		if err := f.m.Start(f.parent); !errors.Is(err, task.ErrStopped) {
			t.Fatalf("Start after Stop = %v, want ErrStopped", err)
		}
		if got := f.fb.mountCount(); got != 0 {
			t.Fatalf("mounts = %d, want 0", got)
		}
	})
}

// A stop while the mount is still blocked: the late mount is unmounted, and
// the cache closes after that.
func TestStopDuringStarting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.gate = make(chan struct{})
		started := f.startAsync()
		synctest.Wait()

		stopped := make(chan error, 1)
		go func() { stopped <- f.stopParent(t, 30*time.Second) }()
		synctest.Wait()
		close(f.fb.gate)

		if err := <-stopped; err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		if err := <-started; err == nil {
			t.Fatal("Start succeeded during a stop")
		}
		requireCalls(t, f, "mount", "unmount", "vfsClose")
		if err := f.m.Stop(); err != nil {
			t.Fatalf("Stop = %v", err)
		}
		requireState(t, f.m, dfsStopped)
	})
}

// Start gives up while the mount is blocked; the mount that comes up late is
// unmounted by the owner and the cache closes after that.
func TestStartTimeoutLateMount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.gate = make(chan struct{})
		started := f.startAsync()

		if err := <-started; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Start = %v, want a timeout", err)
		}
		requireState(t, f.m, dfsFailed)

		close(f.fb.gate)
		synctest.Wait()
		requireCalls(t, f, "mount", "unmount", "vfsClose")
		requireState(t, f.m, dfsFailed)

		if err := f.m.Stop(); err != nil {
			t.Fatalf("Stop = %v", err)
		}
		requireState(t, f.m, dfsStopped)
	})
}

func TestServeEndWhileReadyDies(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		f.fb.handle.end()
		synctest.Wait()

		requireState(t, f.m, dfsFailed)
		if !f.rootFailed() {
			t.Fatal("root group did not fail")
		}
		if cause := f.root.Cause(); !errors.Is(cause, errMountDied) {
			t.Fatalf("root cause = %v, want errMountDied", cause)
		}
		if !strings.Contains(f.logs.String(), "DFS mount died: serve loop ended while mounted") {
			t.Fatalf("no died log in %s", f.logs.String())
		}
	})
}

// Our own unmount ends the serve loop; that must never look like a death.
func TestStopNeverDies(t *testing.T) {
	t.Parallel()
	for i := range 100 {
		t.Run("run"+strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			stopNeverDiesOnce(t)
		})
	}
}

func stopNeverDiesOnce(t *testing.T) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		if err := f.stopParent(t, 30*time.Second); err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		requireState(t, f.m, dfsStopped)
		if f.rootFailed() {
			t.Fatal("root failed during a stop")
		}
		out := f.logs.String()
		if strings.Contains(out, `"to":"failed"`) {
			t.Fatalf("a Failed move happened during a stop:\n%s", out)
		}
		if strings.Contains(out, `"level":"error"`) {
			t.Fatalf("an ERROR was logged during a stop:\n%s", out)
		}
	})
}

func TestServeEndBeforeReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.handle.end()
		if err := f.m.Start(f.parent); err == nil {
			t.Fatal("Start succeeded with a dead serve loop")
		}
		requireState(t, f.m, dfsFailed)
		synctest.Wait()
		if f.rootFailed() {
			t.Fatal("root failed on a start failure")
		}
	})
}

func TestParentStopOrdersUnmountFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		if err := f.stopParent(t, 30*time.Second); err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		requireCalls(t, f, "mount", "unmount", "vfsClose")
		if !strings.Contains(f.logs.String(), `"branch":"graceful"`) {
			t.Fatalf("no graceful branch in %s", f.logs.String())
		}
		start := time.Now()
		if err := f.m.Stop(); err != nil {
			t.Fatalf("Stop = %v", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("Stop after the parent stop took %v", elapsed)
		}
	})
}

func TestGracefulErrorDetaches(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.handle.unmountErr = syscall.EBUSY
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		if err := f.stopParent(t, 30*time.Second); err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		requireCalls(t, f, "mount", "unmount", "detach", "vfsClose")
		out := f.logs.String()
		if !strings.Contains(out, `"branch":"forced"`) || !strings.Contains(out, syscall.EBUSY.Error()) {
			t.Fatalf("forced branch with the graceful error not logged in %s", out)
		}
	})
}

func TestGracefulHangDetachesAt5s(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.handle.hang = make(chan struct{})
		f.fb.handle.detachReleases = true
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		begin := time.Now()
		if err := f.stopParent(t, 30*time.Second); err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		requireCalls(t, f, "mount", "unmount", "detach", "vfsClose")
		if got := f.calls.at("detach").Sub(begin); got != gracefulUnmountTimeout {
			t.Fatalf("detach at +%v, want +%v", got, gracefulUnmountTimeout)
		}
		// The closer bound holds only because the fake's Detach frees the hung
		// Unmount; a real hung fusermount or loops.Wait holds the group until the
		// stopper's deadline.
		if got := f.calls.at("vfsClose").Sub(begin); got > 7*time.Second {
			t.Fatalf("vfsClose at +%v, want by +7s", got)
		}
		if !strings.Contains(f.logs.String(), `"reason":"timeout"`) {
			t.Fatalf("no timeout reason in %s", f.logs.String())
		}
	})
}

// A hung unmount task holds the backend group to the stopper's deadline; the
// closer still runs, and the stop error names the task.
func TestUnmountTaskHangNamesIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.handle.hang = make(chan struct{})
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		err := f.stopParent(t, 8*time.Second)
		if err == nil || !strings.Contains(err.Error(), "backend:unmount") {
			t.Fatalf("parent stop = %v, want an error naming backend:unmount", err)
		}
		f.fb.handle.release()
		synctest.Wait()
		if !slices.Contains(f.calls.names(), "vfsClose") {
			t.Fatalf("the cache never closed: %v", f.calls.names())
		}
	})
}

// If the group refuses the unmount task, the owner still unmounts and the
// cache still closes after it.
func TestUnmountTaskRefused(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.onMount = func(g *task.Group) { g.Close() }
		_ = f.m.Start(f.parent)
		synctest.Wait()

		names := f.calls.names()
		if len(names) < 3 || names[len(names)-1] != "vfsClose" ||
			(!slices.Contains(names, "unmount") && !slices.Contains(names, "detach")) {
			t.Fatalf("calls = %v, want an unmount or detach, then vfsClose", names)
		}
		if f.m.state.Load() == dfsFailed {
			t.Fatal("state failed")
		}
		if f.rootFailed() {
			t.Fatal("root failed")
		}
	})
}

// The serve loop has ended and the stop has begun by the time the owner looks:
// that is a stop, never a death, and the coinciding serve end is logged.
func TestServeEndAtStopIsAStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		ended := newFakeHandle(f.calls)
		ended.end()
		if err := f.m.servingEnded(ctx, ended); err != nil {
			t.Fatalf("servingEnded = %v, want nil (a stop)", err)
		}
		requireState(t, f.m, dfsReady)
		if !strings.Contains(f.logs.String(), "DFS serve loop ended as the stop began") {
			t.Fatalf("no serve-end warning in %s", f.logs.String())
		}
	})
}

// Every move to Failed keeps its cause, and Stats shows it.
func TestFailedStatsCarryCause(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(f *lifecycleFixture)
		// died ends the serve loop after a good start.
		died bool
		want string
	}{
		{name: "start failure", setup: func(f *lifecycleFixture) { f.fb.mountErr = errors.New("boom") }, want: "boom"},
		{
			name: "backend not created",
			setup: func(f *lifecycleFixture) {
				f.m.newBackend = func(*vfs.Manager) (backend.Backend, error) { return nil, errors.New("no fuse") }
			},
			want: "no fuse",
		},
		{
			name:  "serve ended before ready",
			setup: func(f *lifecycleFixture) { f.fb.handle.end() },
			want:  "serve loop ended before the mount was ready",
		},
		{name: "timeout", setup: func(f *lifecycleFixture) { f.fb.gate = make(chan struct{}) }, want: "timed out"},
		{name: "died", setup: func(*lifecycleFixture) {}, died: true, want: "serve loop ended while mounted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				defer f.finish(t)
				tc.setup(f)
				err := <-f.startAsync()
				if tc.died {
					if err != nil {
						t.Fatalf("Start = %v", err)
					}
					f.fb.handle.end()
				}
				synctest.Wait()
				if f.fb.gate != nil {
					close(f.fb.gate)
					synctest.Wait()
				}
				requireState(t, f.m, dfsFailed)
				if got, _ := f.m.Stats()["error"].(string); !strings.Contains(got, tc.want) {
					t.Fatalf(`Stats()["error"] = %q, want it to contain %q`, got, tc.want)
				}
			})
		})
	}
}

// A stop whose unmount never finishes within the budget reports stopping: we
// never saw the mount go, so stopped would be a lie.
func TestTimedOutStopStaysStopping(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.handle.hang = make(chan struct{})
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		if err := f.m.Stop(); err == nil {
			t.Fatal("Stop = nil, want the timeout")
		}
		synctest.Wait()
		if got := f.m.Stats()["state"]; got != "stopping" {
			t.Fatalf(`Stats()["state"] = %v, want "stopping"`, got)
		}
		f.fb.handle.release()
		synctest.Wait()
	})
}

func TestExplicitStopOrders(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		if err := f.m.Stop(); err != nil {
			t.Fatalf("Stop = %v", err)
		}
		requireCalls(t, f, "mount", "unmount", "vfsClose")
		requireState(t, f.m, dfsStopped)
	})
}

// After the stop no DFS task is left under the mount group.
func TestDFSSubtreeEmptyAfterStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		var names []string
		for _, info := range f.parent.Snapshot() {
			names = append(names, info.Path)
		}
		for _, want := range []string{"vfs:evict", "vfs:speed", "vfs:closer", "backend:owner", "backend:unmount"} {
			if !slices.ContainsFunc(names, func(p string) bool { return strings.HasSuffix(p, want) }) {
				t.Fatalf("no task %s in %v", want, names)
			}
		}
		if err := f.stopParent(t, 30*time.Second); err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		if left := f.parent.Snapshot(); len(left) != 0 {
			t.Fatalf("tasks left after the stop: %v", left)
		}
	})
}

// A stop that cancels a Mount waiting for the host is not a failure.
func TestStopDuringStartingMountCancelled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.untilCancelled = true
		started := f.startAsync()
		synctest.Wait()

		if err := f.stopParent(t, 30*time.Second); err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		if err := <-started; err == nil {
			t.Fatal("Start succeeded during a stop")
		}
		if err := f.m.Stop(); err != nil {
			t.Fatalf("Stop = %v", err)
		}
		requireState(t, f.m, dfsStopped)
		out := f.logs.String()
		if strings.Contains(out, `"to":"failed"`) {
			t.Fatalf("a Failed move happened during a stop:\n%s", out)
		}
		if strings.Contains(out, `"level":"error"`) {
			t.Fatalf("an ERROR was logged during a stop:\n%s", out)
		}
	})
}

// armedSlice is a slice armed for d from now, like the generation's mount slice.
func armedSlice(t *testing.T, d time.Duration) task.Slice {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return func() (context.Context, bool) { return ctx, true }
}

// A slice shorter than the graceful timeout cuts the graceful unmount: the
// detach comes with the slice, not at gracefulUnmountTimeout.
func TestDFSStopBoundedBySlice(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.handle.hang = make(chan struct{})
		f.fb.handle.detachReleases = true
		f.m.SetStopSlice(armedSlice(t, 3*time.Second))
		if err := f.m.Start(f.parent); err != nil {
			t.Fatalf("Start = %v", err)
		}
		begin := time.Now()
		if err := f.stopParent(t, 30*time.Second); err != nil {
			t.Fatalf("parent stop = %v", err)
		}
		requireCalls(t, f, "mount", "unmount", "detach", "vfsClose")
		if got, want := f.calls.at("detach").Sub(begin), 3*time.Second; got != want {
			t.Fatalf("detach at +%v, want +%v", got, want)
		}
		// A slice too short for the cache's reserve still leaves the detach a
		// live ctx: a real lazy detach (exec.CommandContext) refuses an ended one.
		if f.fb.handle.detachCtxDone.Load() {
			t.Fatal("detach got an ended ctx")
		}
		got := time.Unix(0, f.fb.handle.detachDeadline.Load()).Sub(begin)
		if want := 3*time.Second + detachFloor; got != want {
			t.Fatalf("detach ctx ends +%v, want +%v", got, want)
		}
	})
}

// A mount that came up after the stop began is released on the constant budget
// when no slice is armed: the detach ctx ends stopBudget-vfsCloseReserve out.
func TestDFSUnmountLateUsesConstantBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		defer f.finish(t)
		f.fb.handle.unmountErr = syscall.EBUSY
		f.fb.onMount = func(g *task.Group) { g.Close() }
		f.m.SetStopSlice(func() (context.Context, bool) { return nil, false })
		begin := time.Now()
		_ = f.m.Start(f.parent)
		synctest.Wait()

		if !slices.Contains(f.calls.names(), "detach") {
			t.Fatalf("calls = %v, want a detach", f.calls.names())
		}
		got := time.Unix(0, f.fb.handle.detachDeadline.Load()).Sub(begin)
		if want := stopBudget - vfsCloseReserve; got != want {
			t.Fatalf("detach ctx ends +%v, want +%v", got, want)
		}
	})
}
