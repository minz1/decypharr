package task_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task"
)

// checkDetached asserts ctx ignores a cancelled parent and ends exactly d from start.
func checkDetached(t *testing.T, ctx context.Context, start time.Time, d time.Duration) {
	t.Helper()
	if ctx.Err() != nil {
		t.Fatalf("ctx done at once: %v", ctx.Err())
	}
	dl, ok := ctx.Deadline()
	if !ok || !dl.Equal(start.Add(d)) {
		t.Fatalf("deadline = %v (ok %v), want %v", dl, ok, start.Add(d))
	}
	time.Sleep(d - time.Nanosecond)
	if ctx.Err() != nil {
		t.Fatalf("ctx done before d: %v", ctx.Err())
	}
	time.Sleep(time.Nanosecond)
	<-ctx.Done()
}

func TestSliceNilIsDetached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		cancel()
		var s task.Slice
		start := time.Now()
		ctx, done := s.Bound(parent, 5*time.Second)
		defer done()
		checkDetached(t, ctx, start, 5*time.Second)
	})
}

func TestSliceUnarmedIsDetached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		s := task.Slice(func() (context.Context, bool) { calls++; return nil, false })
		start := time.Now()
		ctx, done := s.Bound(parent, 5*time.Second)
		defer done()
		if calls != 1 {
			t.Fatalf("slice called %d times, want 1", calls)
		}
		checkDetached(t, ctx, start, 5*time.Second)
	})
}

func TestSliceArmedTakesTheShorter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		for _, tc := range []struct{ slice, d, want time.Duration }{
			{3 * time.Second, 5 * time.Second, 3 * time.Second},
			{8 * time.Second, 5 * time.Second, 5 * time.Second},
		} {
			sctx, cancel := context.WithTimeout(t.Context(), tc.slice)
			s := task.Slice(func() (context.Context, bool) { return sctx, true })
			ctx, done := s.Bound(context.Background(), tc.d)
			dl, ok := ctx.Deadline()
			if !ok || !dl.Equal(start.Add(tc.want)) {
				t.Fatalf("slice %v d %v: deadline %v, want %v", tc.slice, tc.d, dl, start.Add(tc.want))
			}
			<-ctx.Done()
			if got := time.Since(start); got != tc.want {
				t.Fatalf("slice %v d %v: done after %v, want %v", tc.slice, tc.d, got, tc.want)
			}
			done()
			cancel()
			<-sctx.Done()
			start = time.Now()
		}
	})
}
