package repair

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func countingBreaker(probes *atomic.Int64, hold func()) *breaker {
	return newBreaker(
		func() config.BreakerSettings { return config.RepairConfig{}.BreakerSettings() },
		func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
			probes.Add(1)
			hold()
			return canaryPass
		},
	)
}

// A caller that reaches the flight just after another's probe finished finds its verdict cached instead of probing
// again. Run in sequence, so it does not depend on scheduling.
func TestAVerdictThatArrivedWhileQueuedIsNotProbedAgain(t *testing.T) {
	t.Parallel()
	var probes atomic.Int64
	b := countingBreaker(&probes, func() {})
	for range 3 {
		if got := b.probeCanary(t.Context(), testBucket()); got != canaryPass {
			t.Fatalf("verdict = %v", got)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("canary probed %d times for 3 callers inside the cache, want 1", got)
	}
}

// Callers probing one bucket at the same moment share a single probe.
func TestConcurrentProbesOfOneBucketAreShared(t *testing.T) {
	t.Parallel()
	const callers = 8
	var probes atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	b := countingBreaker(&probes, func() {
		once.Do(func() { close(started) })
		<-release
	})
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			b.probeCanary(t.Context(), testBucket())
		})
	}
	<-started
	close(release)
	wg.Wait()
	if got := probes.Load(); got != 1 {
		t.Fatalf("canary probed %d times for %d concurrent callers, want 1", got, callers)
	}
}
