package repair

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// A probe cut off because the service stopped says nothing about the provider: it must not be cached, or every
// repair-settings save would hold the bucket's heals for the cache's length.
func TestACancelledProbeIsNotCached(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lifetime, stop := context.WithCancel(t.Context())
		var probes atomic.Int64
		b := newBreaker(
			func() config.BreakerSettings { return config.RepairConfig{}.BreakerSettings() },
			func(ctx context.Context, _ Bucket, _ config.BreakerSettings) canaryOutcome {
				if probes.Add(1) == 1 {
					<-ctx.Done()
					return canaryInconclusive
				}
				return canaryPass
			},
		)
		b.lifetime = func() context.Context { return lifetime }
		done := make(chan canaryOutcome, 1)
		go func() { done <- b.probeCanary(t.Context(), testBucket()) }()
		synctest.Wait()
		stop()
		if got := <-done; got != canaryAborted {
			t.Fatalf("a cancelled probe = %v, want it reported as aborted", got)
		}

		// The service restarts with a fresh lifetime: the next caller probes again and gets a real answer.
		fresh, cancel := context.WithCancel(t.Context())
		defer cancel()
		b.lifetime = func() context.Context { return fresh }
		if got := b.probeCanary(t.Context(), testBucket()); got != canaryPass || probes.Load() != 2 {
			t.Fatalf("next caller = %v after %d probes, want a fresh pass from a second probe", got, probes.Load())
		}
	})
}

// A cancelled probe holds the evaluation that asked, without tripping the bucket.
func TestAnAbortedProbeHoldsWithoutTripping(t *testing.T) {
	t.Parallel()
	verdict := canaryAborted
	b, _, _ := testBreaker(&verdict)
	out := b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r", confirmed: 1, probed: 100})
	if out.hold != holdCanaryInconclusive || out.newlyTripped {
		t.Fatalf("outcome = %+v, want held as inconclusive and not tripped", out)
	}
	if state := b.bucketLocked(testBucket()).state; state != breakerClosed {
		t.Fatalf("state = %s, want closed", state)
	}
}

// A probe that times out is cached as inconclusive, as ruled: the provider did not answer in time.
func TestATimedOutProbeIsCached(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var probes atomic.Int64
		b := newBreaker(
			func() config.BreakerSettings { return config.RepairConfig{}.BreakerSettings() },
			func(ctx context.Context, _ Bucket, _ config.BreakerSettings) canaryOutcome {
				probes.Add(1)
				<-ctx.Done()
				return canaryInconclusive
			},
		)
		b.lifetime = func() context.Context { return t.Context() }
		start := time.Now()
		for range 2 {
			if got := b.probeCanary(t.Context(), testBucket()); got != canaryInconclusive {
				t.Fatalf("verdict = %v, want inconclusive", got)
			}
		}
		if probes.Load() != 1 || time.Since(start) != canaryProbeTimeout {
			t.Fatalf(
				"probes = %d after %s, want one timed-out probe whose verdict was cached",
				probes.Load(),
				time.Since(start),
			)
		}
	})
}
