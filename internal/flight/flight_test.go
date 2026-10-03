package flight_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/flight"
)

// One caller giving up must not fail the others sharing its call.
func TestCancelledCallerDoesNotFailOthers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var g flight.Group[string]
		release := make(chan struct{})
		var runs atomic.Int32
		fn := func(ctx context.Context) (string, error) {
			runs.Add(1)
			select {
			case <-release:
				return "link", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		firstCtx, cancelFirst := context.WithCancel(t.Context())
		firstErr := make(chan error, 1)
		go func() {
			_, _, err := g.Do(firstCtx, "key", fn)
			firstErr <- err
		}()
		synctest.Wait()
		second := make(chan string, 1)
		go func() {
			val, shared, err := g.Do(t.Context(), "key", fn)
			if err != nil || !shared {
				t.Errorf("second caller: shared = %v, err = %v", shared, err)
			}
			second <- val
		}()
		synctest.Wait()

		cancelFirst()
		if err := <-firstErr; !errors.Is(err, context.Canceled) {
			t.Fatalf("first caller err = %v, want its own cancellation", err)
		}
		close(release)
		if val := <-second; val != "link" {
			t.Fatalf("second caller got %q", val)
		}
		if runs.Load() != 1 {
			t.Fatalf("fn ran %d times, want 1", runs.Load())
		}
	})
}

// When every caller gives up, the call is canceled and a new caller starts
// a fresh one.
func TestLastCallerCancelsTheCall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var g flight.Group[int]
		stopped := make(chan struct{})
		var runs atomic.Int32
		blocking := func(ctx context.Context) (int, error) {
			runs.Add(1)
			<-ctx.Done()
			close(stopped)
			return 0, ctx.Err()
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = g.Do(ctx, "key", blocking)
		}()
		synctest.Wait()
		cancel()
		<-done
		<-stopped

		val, shared, err := g.Do(t.Context(), "key", func(context.Context) (int, error) {
			runs.Add(1)
			return 7, nil
		})
		if err != nil || shared || val != 7 || runs.Load() != 2 {
			t.Fatalf("fresh call: %d, shared %v, %v, runs %d", val, shared, err, runs.Load())
		}
	})
}
