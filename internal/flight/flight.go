// Package flight deduplicates concurrent calls for the same key, like
// golang.org/x/sync/singleflight, without letting one caller's cancellation
// fail the others.
package flight

import (
	"context"
	"sync"
)

// Group runs one call per key at a time and shares its result with every
// caller that asks for the key while it runs.
//
// The shared call runs on a context detached from any single caller: it
// keeps the first caller's values but not its cancellation or deadline, and
// it is canceled only once every waiting caller has given up. Each caller
// still returns as soon as its own context ends. The zero Group is ready.
type Group[T any] struct {
	mu    sync.Mutex
	calls map[string]*call[T]
}

type call[T any] struct {
	done    chan struct{} // closed when fn has returned
	stop    chan struct{} // closed by the last waiter to give up
	waiters int
	val     T
	err     error
}

// Do returns the result of fn for key, running fn unless a call for key is
// already in flight. shared reports whether the result came from a call
// another caller started.
func (g *Group[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, bool, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*call[T])
	}
	c, shared := g.calls[key]
	if !shared {
		c = &call[T]{done: make(chan struct{}), stop: make(chan struct{})}
		g.calls[key] = c
		go g.run(context.WithoutCancel(ctx), key, c, fn)
	}
	c.waiters++
	g.mu.Unlock()

	select {
	case <-c.done:
		return c.val, shared, c.err
	case <-ctx.Done():
		g.leave(key, c)
		var zero T
		return zero, shared, ctx.Err()
	}
}

// run calls fn on a context that ends when every waiter has given up.
func (g *Group[T]) run(base context.Context, key string, c *call[T], fn func(context.Context) (T, error)) {
	defer close(c.done)
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	go func() {
		select {
		case <-c.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	c.val, c.err = fn(ctx)
	g.mu.Lock()
	if g.calls[key] == c {
		delete(g.calls, key)
	}
	g.mu.Unlock()
}

// leave drops a waiter that gave up. The last one cancels the call and
// forgets it, so a later caller starts a fresh call rather than joining a
// canceled one.
func (g *Group[T]) leave(key string, c *call[T]) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c.waiters--
	if c.waiters > 0 {
		return
	}
	close(c.stop)
	if g.calls[key] == c {
		delete(g.calls, key)
	}
}
