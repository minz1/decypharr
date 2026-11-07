package repair

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// Stop cancels a canary probe still running and waits for it to wind down, so none reads storage once the service
// is stopped.
func TestStopWaitsForAProbeInFlight(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	started := make(chan struct{})
	var finished atomic.Bool
	service.breaker.canary = func(ctx context.Context, _ Bucket, _ config.BreakerSettings) canaryOutcome {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // winding down after the cancellation
		finished.Store(true)
		return canaryInconclusive
	}
	go service.QueueCanary(context.WithoutCancel(t.Context()), "rd")
	<-started

	service.Stop()
	if !finished.Load() {
		t.Fatal("Stop returned while a canary probe was still running")
	}
}

// A service restarted by a config change probes again: Stop's cancellation ends with that stop.
func TestAProbeAfterARestartIsNotCancelled(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	service.Stop()
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var cancelled atomic.Bool
	service.breaker.canary = func(ctx context.Context, _ Bucket, _ config.BreakerSettings) canaryOutcome {
		cancelled.Store(ctx.Err() != nil)
		return canaryPass
	}
	service.QueueCanary(t.Context(), "rd")
	if cancelled.Load() {
		t.Fatal("a probe after Stop and Start ran on a cancelled context")
	}
}
