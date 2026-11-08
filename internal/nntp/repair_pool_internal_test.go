package nntp

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

const repairStopBudget = 5 * time.Second

type statFunc = func(ctx context.Context, ids []string) ([]StatResult, error)

func newTestRepairPool(g *task.Group, capacity int, stat statFunc) *RepairPool {
	return &RepairPool{g: g, tokens: make(chan struct{}, capacity), stat: stat}
}

func stopRepairGroup(t *testing.T, g *task.Group) {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), repairStopBudget)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// blockingStat signals started, then holds the chunk until its ctx ends.
func blockingStat(started chan<- struct{}) statFunc {
	return func(ctx context.Context, _ []string) ([]StatResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// recordDone returns a done callback that records every call.
func recordDone(calls chan<- error) func([]StatResult, error) {
	return func(_ []StatResult, err error) { calls <- err }
}

// A Submit waiting for capacity is released by the stop, refused, and its done
// never runs; the chunk already running ends with its ctx and calls done once.
func TestRepairSubmitBlockedOnCapacityReturnsClosedOnStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := task.NewRoot(t.Context(), "test", zerolog.Nop())
		started := make(chan struct{}, 2)
		p := newTestRepairPool(g, 1, blockingStat(started))

		first := make(chan error, 2)
		if err := p.Submit(t.Context(), []string{"a@b"}, recordDone(first)); err != nil {
			t.Fatal(err)
		}
		<-started

		second := make(chan error, 2)
		submitted := make(chan error, 1)
		go func() { submitted <- p.Submit(t.Context(), []string{"c@d"}, recordDone(second)) }()
		synctest.Wait()
		select {
		case err := <-submitted:
			t.Fatalf("second Submit returned %v while the pool was full", err)
		default:
		}

		stopRepairGroup(t, g)
		if err := <-submitted; !errors.Is(err, errRepairPoolClosed) {
			t.Fatalf("second Submit = %v, want errRepairPoolClosed", err)
		}
		if err := <-first; !errors.Is(err, context.Canceled) {
			t.Fatalf("first done err = %v, want context.Canceled", err)
		}
		synctest.Wait()
		if len(first) != 0 {
			t.Fatal("first done ran more than once")
		}
		if len(second) != 0 {
			t.Fatal("the refused chunk's done ran")
		}
	})
}

func TestRepairSubmitAfterStop(t *testing.T) {
	t.Parallel()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	stopRepairGroup(t, g)
	p := newTestRepairPool(g, 1, func(context.Context, []string) ([]StatResult, error) {
		t.Error("a chunk ran after stop")
		return nil, nil
	})
	calls := make(chan error, 1)
	if err := p.Submit(t.Context(), []string{"a@b"}, recordDone(calls)); !errors.Is(err, errRepairPoolClosed) {
		t.Fatalf("Submit after stop = %v, want errRepairPoolClosed", err)
	}
	if len(calls) != 0 {
		t.Fatal("done ran for a refused chunk")
	}
	if held := len(p.tokens); held != 0 {
		t.Fatalf("refused Submit holds %d tokens", held)
	}
}

// A Submit whose ctx is already done (BatchStat bailed out) spawns no chunk.
func TestRepairSubmitCancelledCtxSpawnsNoChunk(t *testing.T) {
	t.Parallel()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() { stopRepairGroup(t, g) })
	p := newTestRepairPool(g, 1, func(context.Context, []string) ([]StatResult, error) {
		t.Error("a chunk ran for a cancelled Submit")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := make(chan error, 1)
	if err := p.Submit(ctx, []string{"a@b"}, recordDone(calls)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit with a cancelled ctx = %v, want context.Canceled", err)
	}
	if len(calls) != 0 || len(p.tokens) != 0 || len(g.Snapshot()) != 0 {
		t.Fatalf("cancelled Submit left work: done calls %d, tokens %d, tasks %v",
			len(calls), len(p.tokens), g.Snapshot())
	}
}

// A chunk that panics still calls done and gives its token back.
func TestRepairChunkPanicStillCallsDone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := task.NewRoot(t.Context(), "test", zerolog.Nop())
		calls := 0
		p := newTestRepairPool(g, 1, func(context.Context, []string) ([]StatResult, error) {
			calls++
			if calls == 1 {
				panic("boom")
			}
			return []StatResult{{MessageID: "c@d", Available: true}}, nil
		})

		panicked := make(chan error, 2)
		if err := p.Submit(t.Context(), []string{"a@b"}, recordDone(panicked)); err != nil {
			t.Fatal(err)
		}
		if err := <-panicked; err == nil {
			t.Fatal("a panicking chunk reported no error")
		}

		ok := make(chan error, 1)
		if err := p.Submit(t.Context(), []string{"c@d"}, recordDone(ok)); err != nil {
			t.Fatalf("Submit after a panicked chunk = %v: its token was not released", err)
		}
		if err := <-ok; err != nil {
			t.Fatalf("second chunk err = %v", err)
		}
		stopRepairGroup(t, g)
		if len(panicked) != 0 {
			t.Fatal("the panicking chunk's done ran twice")
		}
	})
}

func TestRepairStopLeavesNoTasks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := task.NewRoot(t.Context(), "test", zerolog.Nop())
		const chunks = 3
		started := make(chan struct{}, chunks)
		p := newTestRepairPool(g, chunks, blockingStat(started))
		calls := make(chan error, 2*chunks)
		for range chunks {
			if err := p.Submit(t.Context(), []string{"a@b"}, recordDone(calls)); err != nil {
				t.Fatal(err)
			}
		}
		for range chunks {
			<-started
		}
		if n := len(g.Snapshot()); n != chunks {
			t.Fatalf("running tasks = %v, want %d chunks", g.Snapshot(), chunks)
		}

		stopRepairGroup(t, g)
		if left := g.Snapshot(); len(left) != 0 {
			t.Fatalf("tasks left after stop: %v", left)
		}
		if n := len(calls); n != chunks {
			t.Fatalf("done calls = %d, want %d", n, chunks)
		}
		if held := len(p.tokens); held != 0 {
			t.Fatalf("tokens still held after stop: %d", held)
		}
	})
}
