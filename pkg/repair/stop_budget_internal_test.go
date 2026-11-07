package repair

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// Waiting for a stuck probe and for a stuck run share one drain budget, so a settings save never waits twice as long.
func TestStopSharesOneDrainBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		service := New(Dependencies{})
		block := make(chan struct{})
		started := make(chan struct{})
		service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
			close(started)
			<-block // a probe that ignores its cancellation
			return canaryInconclusive
		}
		service.runWG.Go(func() { <-block }) // a run that does not drain either
		go service.QueueCanary(t.Context(), "rd")
		<-started

		begin := time.Now()
		service.Stop()
		if waited := time.Since(begin); waited != repairStopDrainTimeout {
			t.Fatalf("Stop waited %s with a stuck probe and a stuck run, want one budget of %s",
				waited, repairStopDrainTimeout)
		}
		close(block)
		synctest.Wait()
	})
}
