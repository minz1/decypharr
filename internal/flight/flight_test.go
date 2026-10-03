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

// A call every caller abandoned is not canceled: it runs to completion
// (bounded by fn's own timeouts), and a caller arriving meanwhile shares it.
func TestAbandonedCallRunsToCompletion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var g flight.Group[int]
		release := make(chan struct{})
		var runs atomic.Int32
		fn := func(ctx context.Context) (int, error) {
			runs.Add(1)
			select {
			case <-release:
				return 7, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, _, err := g.Do(ctx, "key", fn)
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("abandoning caller err = %v, want its cancellation", err)
		}

		later := make(chan int, 1)
		go func() {
			val, _, err := g.Do(t.Context(), "key", fn)
			if err != nil {
				t.Errorf("later caller: %v", err)
			}
			later <- val
		}()
		synctest.Wait()
		close(release)
		if val := <-later; val != 7 || runs.Load() != 1 {
			t.Fatalf("later caller got %d after %d runs, want the abandoned call's 7 from 1 run", val, runs.Load())
		}
	})
}
