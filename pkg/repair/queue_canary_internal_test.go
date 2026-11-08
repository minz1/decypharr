package repair

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

// QueueCanary hands queue cleanup the breaker's verdict under the arr name for it. An aborted probe in particular
// must stay aborted: queue cleanup reads an inconclusive verdict as the provider failing to answer.
func TestQueueCanaryMapsEachVerdict(t *testing.T) {
	t.Parallel()
	answered := []struct {
		outcome canaryOutcome
		want    arr.CanaryVerdict
	}{
		{canaryPass, arr.CanaryPass},
		{canaryFail, arr.CanaryFail},
		{canaryInconclusive, arr.CanaryInconclusive},
		{canaryNone, arr.CanaryNone},
	}
	for _, tc := range answered {
		service := New(testGroup(t), Dependencies{})
		service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
			return tc.outcome
		}
		if got := service.QueueCanary(t.Context(), "rd"); got != tc.want {
			t.Errorf("QueueCanary with a %v probe = %v, want %v", tc.outcome, got, tc.want)
		}
	}

	synctest.Test(t, func(t *testing.T) {
		service := New(testGroup(t), Dependencies{})
		started := make(chan struct{})
		service.breaker.canary = func(ctx context.Context, _ Bucket, _ config.BreakerSettings) canaryOutcome {
			close(started)
			<-ctx.Done()
			return canaryInconclusive
		}
		done := make(chan arr.CanaryVerdict, 1)
		go func() { done <- service.QueueCanary(context.WithoutCancel(t.Context()), "rd") }()
		<-started
		service.Stop()
		if got := <-done; got != arr.CanaryAborted {
			t.Fatalf("QueueCanary cut off by Stop = %v, want %v", got, arr.CanaryAborted)
		}
	})
}
