package repair

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// blockingBreaker has a canary that waits until its context ends, as a probe of a stuck provider would, and a
// lifetime the test can end.
func blockingBreaker(lifetime context.Context, ended chan<- error) *breaker {
	b := newBreaker(
		func() config.BreakerSettings { return config.RepairConfig{}.BreakerSettings() },
		func(ctx context.Context, _ Bucket, _ config.BreakerSettings) canaryOutcome {
			<-ctx.Done()
			ended <- ctx.Err()
			return canaryInconclusive
		},
	)
	b.lifetime = func() context.Context { return lifetime }
	return b
}

// A probe that never answers is cut off, so the flight cannot hang on it forever.
func TestCanaryProbeIsBoundedByATimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ended := make(chan error, 1)
		b := blockingBreaker(t.Context(), ended)
		start := time.Now()
		if got := b.probeCanary(t.Context(), testBucket()); got != canaryInconclusive {
			t.Fatalf("verdict = %v, want inconclusive", got)
		}
		if err := <-ended; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the probe ended with %v, want its deadline", err)
		}
		if waited := time.Since(start); waited != canaryProbeTimeout {
			t.Fatalf("waited %s, want %s", waited, canaryProbeTimeout)
		}
	})
}

// A probe in flight ends with the service's lifetime, so it cannot read storage after shutdown.
func TestCanaryProbeEndsWithTheServiceLifetime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lifetime, stop := context.WithCancel(t.Context())
		ended := make(chan error, 1)
		b := blockingBreaker(lifetime, ended)
		done := make(chan canaryOutcome, 1)
		go func() { done <- b.probeCanary(t.Context(), testBucket()) }()
		synctest.Wait()
		stop()
		if err := <-ended; !errors.Is(err, context.Canceled) {
			t.Fatalf("the probe ended with %v, want it cancelled", err)
		}
		if got := <-done; got != canaryAborted {
			t.Fatalf("verdict = %v, want aborted", got)
		}
	})
}
