package task_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task"
)

func TestGraceOutlivesParentByD(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithCancel(t.Context())
		ctx, cancel := task.Grace(parent, 5*time.Second)
		defer cancel()

		time.Sleep(10 * time.Second)
		stop()
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("grace ctx ended with its parent: %v", ctx.Err())
		}

		time.Sleep(5*time.Second - time.Nanosecond)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("grace ctx ended before parent end + d: %v", ctx.Err())
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("grace ctx err = %v at parent end + d, want Canceled", ctx.Err())
		}
		if !errors.Is(context.Cause(ctx), task.ErrGraceExpired) {
			t.Fatalf("grace ctx cause = %v, want ErrGraceExpired", context.Cause(ctx))
		}
	})
}

func TestGraceNoDeadlineWhileParentLive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := task.Grace(t.Context(), 5*time.Second)
		defer cancel()

		if _, ok := ctx.Deadline(); ok {
			t.Fatal("grace ctx has a deadline while its parent is live")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("grace ctx ended while its parent is live: %v", ctx.Err())
		}
	})
}

// afterFuncCtx is a parent ctx that counts the AfterFunc registrations made
// on it and the stops of those registrations. The context package delegates
// to a ctx's own AfterFunc method only when the ctx is not built on one of
// its cancel contexts, so this one carries its own done channel.
type afterFuncCtx struct {
	mu   sync.Mutex
	done chan struct{}
	fns  map[int]func()
	next int

	registered, stopped atomic.Int32
}

func newAfterFuncCtx() *afterFuncCtx {
	return &afterFuncCtx{done: make(chan struct{}), fns: map[int]func(){}}
}

func (c *afterFuncCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *afterFuncCtx) Done() <-chan struct{}       { return c.done }
func (c *afterFuncCtx) Value(any) any               { return nil }

func (c *afterFuncCtx) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *afterFuncCtx) AfterFunc(f func()) func() bool {
	c.registered.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	c.fns[id] = f
	return func() bool {
		c.stopped.Add(1)
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.fns[id]
		delete(c.fns, id)
		return ok
	}
}

func (c *afterFuncCtx) cancel() {
	c.mu.Lock()
	close(c.done)
	fns := c.fns
	c.fns = map[int]func(){}
	c.mu.Unlock()
	for _, f := range fns {
		go f()
	}
}

func TestGraceCancelFuncReleases(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent := newAfterFuncCtx()
		stop := parent.cancel
		ctx, cancel := task.Grace(parent, 5*time.Second)
		if got := parent.registered.Load(); got != 1 {
			t.Fatalf("Grace registered %d AfterFuncs on its parent, want 1", got)
		}

		cancel()
		if got := parent.stopped.Load(); got != 1 {
			t.Fatalf("the CancelFunc stopped %d AfterFunc registrations, want 1", got)
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("grace ctx err = %v after its CancelFunc, want Canceled", ctx.Err())
		}
		if errors.Is(context.Cause(ctx), task.ErrGraceExpired) {
			t.Fatal("a CancelFunc cancel reads as an expired grace")
		}
		// The parent ending later must arm nothing: no grace timer fires.
		stop()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if errors.Is(context.Cause(ctx), task.ErrGraceExpired) {
			t.Fatal("a grace timer fired after the CancelFunc")
		}
	})
}

func TestGraceCancelFuncAfterParentEnd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithCancel(t.Context())
		ctx, cancel := task.Grace(parent, 5*time.Second)

		stop()
		synctest.Wait()
		cancel()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("grace ctx err = %v after its CancelFunc, want Canceled", ctx.Err())
		}
	})
}
