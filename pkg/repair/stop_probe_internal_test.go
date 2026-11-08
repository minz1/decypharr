package repair

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/config"
)

// A canary probe in flight is a lease on the group: the group is not done until the probe has left, so none reads
// storage once the service is stopped.
func TestStopWaitsForAProbeInFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		service := New(testGroup(t), Dependencies{})
		started := make(chan struct{})
		wind := make(chan struct{})
		service.breaker.canary = func(ctx context.Context, _ Bucket, _ config.BreakerSettings) canaryOutcome {
			close(started)
			<-ctx.Done()
			<-wind // winding down after the cancellation
			return canaryInconclusive
		}
		go service.QueueCanary(context.WithoutCancel(t.Context()), "rd")
		<-started

		service.Stop()
		service.g.Close()
		synctest.Wait()
		select {
		case <-service.g.Done():
			t.Fatal("the group was done while a canary probe was still running")
		default:
		}
		close(wind)
		<-service.g.Done()
	})
}

// A service whose settings were reapplied probes again: ApplyConfig's cancellation ends with that call.
func TestAProbeAfterApplyConfigIsNotCancelled(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	if err := service.ApplyConfig(); err != nil {
		t.Fatal(err)
	}
	var cancelled atomic.Bool
	service.breaker.canary = func(ctx context.Context, _ Bucket, _ config.BreakerSettings) canaryOutcome {
		cancelled.Store(ctx.Err() != nil)
		return canaryPass
	}
	service.QueueCanary(t.Context(), "rd")
	if cancelled.Load() {
		t.Fatal("a probe after ApplyConfig ran on a cancelled context")
	}
}

// A probe the stopped group refuses never runs, and what it did not learn is not cached.
func TestProbeRefusedAfterStopIsAborted(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	var probes atomic.Int32
	service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
		probes.Add(1)
		return canaryPass
	}
	service.g.Close()
	<-service.g.Done()
	key := Bucket{Provider: "rd", Class: ClassHosterLink}
	if got := service.breaker.probeCanary(t.Context(), key); got != canaryAborted {
		t.Fatalf("probe on a stopped group = %v, want canaryAborted", got)
	}
	if probes.Load() != 0 {
		t.Fatal("a canary probe ran on a stopped group")
	}
	service.breaker.mu.Lock()
	_, cached := service.breaker.verdicts[key]
	service.breaker.mu.Unlock()
	if cached {
		t.Fatal("a refused probe's verdict was cached")
	}
}
