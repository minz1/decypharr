package repair

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// Concurrent evaluations of one bucket probe its canary once and share the verdict.
func TestConcurrentEvaluationsShareOneCanary(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	b := newBreaker(
		func() config.BreakerSettings { return config.RepairConfig{}.BreakerSettings() },
		func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return canaryPass
		},
	)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			b.evaluate(t.Context(), testBucket(), bucketInput{confirmed: 1, probed: -1})
		})
	}
	<-started
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("canary probed %d times for 8 concurrent evaluations, want 1", got)
	}
}
