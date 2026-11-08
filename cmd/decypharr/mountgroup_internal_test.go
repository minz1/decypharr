package decypharr

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

// eventLog records events from many tasks, in order.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// syncBuffer is a log sink many tasks can write to.
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

// fakeMount is a mount manager whose Start and Stop are the test's.
type fakeMount struct {
	manager.MountManager

	log     *eventLog
	startFn func(g *task.Group) error
	starts  atomic.Int32
	stops   atomic.Int32
}

func (f *fakeMount) Start(g *task.Group) error {
	f.starts.Add(1)
	f.log.add("start")
	if f.startFn != nil {
		return f.startFn(g)
	}
	return nil
}

func (f *fakeMount) Stop() error {
	f.stops.Add(1)
	f.log.add("stopped mount")
	return nil
}

// sliceMount is a fakeMount that takes the stop slice, as the real owners do.
type sliceMount struct {
	*fakeMount

	got atomic.Pointer[task.Slice]
	// atStart is whether the slice was set when Start ran.
	atStart atomic.Bool
}

func (s *sliceMount) SetStopSlice(slice task.Slice) { s.got.Store(&slice) }

func (s *sliceMount) Start(g *task.Group) error {
	s.atStart.Store(s.got.Load() != nil)
	return s.fakeMount.Start(g)
}

// mountRig is a generation root with a gate and a slice, inside a synctest bubble.
type mountRig struct {
	t     *testing.T
	root  *task.Group
	gen   *generation
	gate  *manager.MountGate
	slice *mountSlice
	logs  *syncBuffer
	log   *eventLog
	// backend is the WebDAV server's stop channel start hands over; nil by default.
	backend <-chan struct{}
}

func newMountRig(t *testing.T) *mountRig {
	t.Helper()
	logs := &syncBuffer{}
	logger := zerolog.New(logs)
	root := task.NewRoot(t.Context(), "generation", logger)
	gen := newGeneration(root, logger)
	return &mountRig{
		t: t, root: root, gen: gen, logs: logs, log: &eventLog{},
		gate:  manager.NewMountGate(logger),
		slice: newMountSlice(gen),
	}
}

func (r *mountRig) start(mm manager.MountManager, ready <-chan struct{}) {
	r.t.Helper()
	logger := zerolog.New(r.logs)
	if err := startMountGroup(r.root, mm, ready, r.gate, r.slice, r.backend, logger); err != nil {
		r.t.Fatal(err)
	}
}

func (r *mountRig) stop() {
	r.t.Helper()
	ctx, cancel := task.Detached(r.t.Context(), 30*time.Second)
	defer cancel()
	if err := r.root.Stop(ctx); err != nil {
		r.t.Fatalf("stop: %v", err)
	}
}

// child makes a root child with one task that records when it stops.
func (r *mountRig) child(name string) {
	r.t.Helper()
	if err := r.root.Child(name).Go("serve", func(ctx context.Context) error {
		<-ctx.Done()
		r.log.add("stopped " + name)
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

// The gate closes before the mount's own tasks stop: the "stopping" child is
// newer than "run", so it stops first.
func TestMountGroupClosesGateBeforeRunChildrenStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newMountRig(t)
		var sawGate atomic.Bool
		ready := make(chan struct{})
		close(ready)
		r.start(&fakeMount{log: r.log, startFn: func(g *task.Group) error {
			return g.Child("x").Go("watch", func(ctx context.Context) error {
				<-ctx.Done()
				sawGate.Store(r.gate.IsStopping())
				return nil
			})
		}}, ready)
		synctest.Wait()

		r.stop()

		if !sawGate.Load() {
			t.Fatal("the gate was still open when the mount's tasks stopped")
		}
	})
}

// The generation stops share, then mount, then http, then manager; the mount
// starts only after the manager is ready.
func TestMountGroupStopOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newMountRig(t)
		r.child("manager")
		r.child("http")
		mm := &fakeMount{log: r.log}
		ready := make(chan struct{})
		r.start(mm, ready)
		r.child("share")
		synctest.Wait()
		if got := mm.starts.Load(); got != 0 {
			t.Fatalf("mount started %d times before the manager was ready", got)
		}

		close(ready)
		synctest.Wait()
		if got := mm.starts.Load(); got != 1 {
			t.Fatalf("mount starts = %d after ready, want 1", got)
		}
		r.stop()

		want := []string{"start", "stopped share", "stopped mount", "stopped http", "stopped manager"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
	})
}

// A manager that never becomes ready never starts the mount, and the stop does
// not wait for it.
func TestMountGroupStopsWhenManagerNeverReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newMountRig(t)
		mm := &fakeMount{log: r.log}
		r.start(mm, make(chan struct{}))
		synctest.Wait()

		began := time.Now()
		r.stop()

		if got := time.Since(began); got != 0 {
			t.Fatalf("stop took %v, want it prompt", got)
		}
		if mm.starts.Load() != 0 || mm.stops.Load() != 1 {
			t.Fatalf("starts = %d, stops = %d; want 0 and 1", mm.starts.Load(), mm.stops.Load())
		}
	})
}

// An unarmed slice never touches the generation's stop context: asking for it
// would start the drain timer.
func TestMountSliceUnarmedLeavesStopContextAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newMountRig(t)
		r.gen.deadline = 5 * time.Second

		if ctx, ok := r.slice.get(); ok || ctx != nil {
			t.Fatalf("get before begin = (%v, %v), want (nil, false)", ctx, ok)
		}
		if r.gen.stopCtx != nil {
			t.Fatal("get before begin created the generation's stop context")
		}

		r.slice.begin()
		ctx, ok := r.slice.get()
		if !ok {
			t.Fatal("get after begin is not armed")
		}
		sliceEnd, _ := ctx.Deadline()
		genEnd, _ := r.gen.stopCtx.Deadline()
		if sliceEnd.After(genEnd) || time.Until(sliceEnd) > mountSliceBudget {
			t.Fatalf(
				"slice ends in %v, generation in %v; want both within bounds",
				time.Until(sliceEnd),
				time.Until(genEnd),
			)
		}
		r.slice.release()
		if ctx.Err() == nil {
			t.Fatal("release did not end the slice")
		}
	})
}

// The slice reaches an owner that takes one, before Start.
func TestMountSliceHandedToOwnersThatTakeIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newMountRig(t)
		mm := &sliceMount{fakeMount: &fakeMount{log: r.log}}
		ready := make(chan struct{})
		close(ready)
		r.start(mm, ready)
		synctest.Wait()

		if !mm.atStart.Load() {
			t.Fatal("the owner had no stop slice when Start ran")
		}
		got := mm.got.Load()
		if got == nil || *got == nil {
			t.Fatal("the owner's slice is nil")
		}
		if _, armed := (*got)(); armed {
			t.Fatal("the slice is armed before the stop began")
		}
		r.stop()
		if _, armed := (*got)(); !armed {
			t.Fatal("the slice is not armed after the stop")
		}
	})
}

// backendMount is a fakeMount that takes the WebDAV server's stop channel, as the rclone owner does.
type backendMount struct {
	*fakeMount

	got atomic.Pointer[<-chan struct{}]
	// atStart is whether the channel was set when Start ran.
	atStart atomic.Bool
}

func (b *backendMount) SetBackendStopped(stopped <-chan struct{}) { b.got.Store(&stopped) }

func (b *backendMount) Start(g *task.Group) error {
	b.atStart.Store(b.got.Load() != nil)
	return b.fakeMount.Start(g)
}

// The WebDAV server's stop channel reaches an owner that takes one, before Start.
func TestStartMountGroupHandsOverBackendStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newMountRig(t)
		backend := make(chan struct{})
		r.backend = backend
		mm := &backendMount{fakeMount: &fakeMount{log: r.log}}
		ready := make(chan struct{})
		close(ready)
		r.start(mm, ready)
		synctest.Wait()

		if !mm.atStart.Load() {
			t.Fatal("the owner had no backend channel when Start ran")
		}
		if got := mm.got.Load(); got == nil || *got != (<-chan struct{})(backend) {
			t.Fatal("the owner got a different channel than the one passed in")
		}
		r.stop()
	})
}

// A Start that panics is logged and treated as a start failure; Stop still runs.
func TestMountGroupStartPanicStillStops(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newMountRig(t)
		mm := &fakeMount{log: r.log, startFn: func(*task.Group) error { panic("boom") }}
		ready := make(chan struct{})
		close(ready)
		r.start(mm, ready)
		synctest.Wait()

		r.stop()

		if got := mm.stops.Load(); got != 1 {
			t.Fatalf("stops = %d, want 1", got)
		}
		out := r.logs.String()
		if !strings.Contains(out, "boom") ||
			!strings.Contains(out, "Failed to start mount manager, continuing without mounting") {
			t.Fatalf("the panic was not logged as a start failure: %s", out)
		}
	})
}

// A stop that arrives during a slow Start calls Stop once, after Start returned.
func TestMountGroupStopAfterStartReturns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newMountRig(t)
		mm := &fakeMount{log: r.log, startFn: func(g *task.Group) error {
			<-g.Context().Done()
			r.log.add("start returned")
			return context.Cause(g.Context())
		}}
		ready := make(chan struct{})
		close(ready)
		r.start(mm, ready)
		synctest.Wait()

		r.stop()

		want := []string{"start", "start returned", "stopped mount"}
		if got := r.log.list(); !slices.Equal(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		if got := mm.stops.Load(); got != 1 {
			t.Fatalf("stops = %d, want 1", got)
		}
	})
}
