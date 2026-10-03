// Package flight deduplicates concurrent calls for the same key, like
// golang.org/x/sync/singleflight, without letting one caller's cancellation
// fail the others.
package flight

import (
	"context"

	"golang.org/x/sync/singleflight"
)

// Group runs one call per key at a time and shares its result with every
// caller that asks for the key while it runs. The zero Group is ready.
//
// The shared call runs on the first caller's context without its
// cancellation or deadline, so it is never failed by one caller giving up;
// each caller still returns as soon as its own context ends. A call every
// caller abandoned runs to completion, bounded by fn's own HTTP or NNTP
// timeouts.
type Group[T any] struct {
	group singleflight.Group
}

// Do returns the result of fn for key, running fn unless a call for key is
// already in flight. shared reports whether the result was shared with
// other callers.
func (g *Group[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, bool, error) {
	detached := context.WithoutCancel(ctx)
	results := g.group.DoChan(key, func() (any, error) { return fn(detached) })
	select {
	case res := <-results:
		val, _ := res.Val.(T)
		return val, res.Shared, res.Err
	case <-ctx.Done():
		var zero T
		return zero, false, ctx.Err()
	}
}
