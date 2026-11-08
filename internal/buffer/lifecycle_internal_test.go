package buffer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

const lifecycleStopBudget = 30 * time.Second

func newTestGroup(t testing.TB) *task.Group {
	t.Helper()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), lifecycleStopBudget)
		defer cancel()
		_ = g.Stop(ctx)
	})
	return g
}

func stopGroup(t testing.TB, g *task.Group) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), lifecycleStopBudget)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func chargedRelease(t testing.TB, p *Pool) blockRelease {
	t.Helper()
	p.memAllocated.Add(blockSize)
	return blockRelease{data: mmapAlloc(blockSize), pool: p}
}

// A release made while the worker is running is queued, not run by the caller.
func TestUnmapperReleaseBeforeStopIsQueued(t *testing.T) {
	t.Parallel()
	p := &Pool{}
	u := &unmapper{ch: make(chan blockRelease, 1)}
	u.release(chargedRelease(t, p))
	if got := len(u.ch); got != 1 {
		t.Fatalf("queued releases = %d, want 1", got)
	}
	if got := p.memAllocated.Load(); got != blockSize {
		t.Fatalf("queued release ran before the worker: allocated %d", got)
	}
}

// After the worker stopped, release runs the unmap before it returns.
func TestUnmapperReleaseAfterStopRunsInline(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	p, err := NewPool(g, PoolConfig{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	stopGroup(t, g)
	p.unmap.release(chargedRelease(t, p))
	if got := p.memAllocated.Load(); got != 0 {
		t.Fatalf("release after stop left %d bytes charged", got)
	}
}

// A full queue falls back to an inline release.
func TestUnmapperReleaseFullQueueRunsInline(t *testing.T) {
	t.Parallel()
	p := &Pool{}
	u := &unmapper{ch: make(chan blockRelease, 1)}
	u.release(chargedRelease(t, p))
	u.release(chargedRelease(t, p))
	if got := p.memAllocated.Load(); got != blockSize {
		t.Fatalf("allocated %d, want the queued block only", got)
	}
}

func TestPoolStopLeavesNoTasks(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	p, err := NewPool(g, PoolConfig{Name: "test", MemoryBudget: 8 << 20, DiskLimit: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(g.Snapshot()); got != 4 {
		t.Fatalf("running tasks = %v, want mem-evict, disk-evict, unmapper, buffers-closer", g.Snapshot())
	}
	b, err := p.NewBuffer(Config{MemorySize: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	stopGroup(t, g)
	if left := g.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %v", left)
	}
	if got := p.Stats().Buffers; got != 0 {
		t.Fatalf("buffers registered after stop: %d", got)
	}
	if _, writeErr := b.WriteAt([]byte("x"), 0); !errors.Is(writeErr, ErrClosed) {
		t.Fatalf("write to a swept buffer: %v, want ErrClosed", writeErr)
	}
	if _, bufErr := p.NewBuffer(Config{MemorySize: 4 << 20}); !errors.Is(bufErr, ErrClosed) {
		t.Fatalf("NewBuffer after stop: %v, want ErrClosed", bufErr)
	}
}

func TestNewBufferAfterStopRefused(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	p, err := NewPool(g, PoolConfig{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	stopGroup(t, g)
	if _, bufErr := p.NewBuffer(Config{MemorySize: 4 << 20}); !errors.Is(bufErr, ErrClosed) {
		t.Fatalf("NewBuffer after stop: %v, want ErrClosed", bufErr)
	}
	if got := p.Stats().Buffers; got != 0 {
		t.Fatalf("refused buffer registered: %d", got)
	}
}

func TestNewPoolOnStoppedGroupRefused(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	stopGroup(t, g)
	if _, err := NewPool(g, PoolConfig{Name: "test", MemoryBudget: 8 << 20}); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("NewPool on a stopped group: %v, want ErrStopped", err)
	}
}
