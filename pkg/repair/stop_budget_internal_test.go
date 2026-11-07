package repair

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

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

// A probe that uses up the budget is reported as the probe, not as a run that failed to drain: with no run in
// flight there is nothing left to drain. Stop is repeated because the bug was a select between two ready channels.
func TestStopBlamesOnlyWhatTimedOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for range 32 {
			var logs bytes.Buffer
			service := New(Dependencies{Logger: zerolog.New(&logs)})
			block := make(chan struct{})
			started := make(chan struct{})
			service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
				close(started)
				<-block // a probe that ignores its cancellation
				return canaryInconclusive
			}
			go service.QueueCanary(t.Context(), "rd")
			<-started

			service.Stop()
			close(block)
			synctest.Wait()
			if !strings.Contains(logs.String(), "a canary probe did not stop in time") {
				t.Fatalf("Stop logged %q, want the stuck probe named", logs.String())
			}
			if strings.Contains(logs.String(), "drain timed out") {
				t.Fatalf("Stop logged %q with no run in flight, want no drain timeout", logs.String())
			}
		}
	})
}
