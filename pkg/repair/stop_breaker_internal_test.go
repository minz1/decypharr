package repair

import (
	"context"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// stopAfterChecks cancels the sweep context at the n-th probe of the next sweep and marks the stop as scheduled.
func (f *outageFixture) stopAfterChecks(t *testing.T, n int) (context.Context, *repairStopState) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	f.client.mu.Lock()
	start := f.client.checks
	f.client.onCheck = func(checks int) {
		if checks-start == n {
			cancel()
		}
	}
	f.client.mu.Unlock()
	stop := &repairStopState{}
	stop.set()
	return ctx, stop
}

func TestStopScheduleFinalHealPassesTheBreaker(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{BreakerMinConfirmed: 5, BreakerPercent: 50})
	f.downRange(0, 8)
	f.sweep(t.Context(), nil)

	ctx, stop := f.stopAfterChecks(t, 8)
	run := f.sweep(ctx, stop)
	if run.Status != storage.RepairRunCompleted || run.Stats.Probed != 8 {
		t.Fatalf(
			"run = %s probed %d, want a stopped-by-schedule completion after the 8 probes",
			run.Status,
			run.Stats.Probed,
		)
	}
	if f.requests() != 0 {
		t.Fatalf("the final heal healed %d entries of a bucket past threshold", f.requests())
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 8 {
		t.Fatalf("held = %d, want 8", got)
	}
}

func TestStopScheduleFinalHealStillHealsWhatTheBreakerAdmits(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(3, 4)
	f.sweep(t.Context(), nil)

	ctx, stop := f.stopAfterChecks(t, 1)
	f.sweep(ctx, stop)
	if f.requests() != 1 || f.reacquirer.requests[0].FileID != "id-release-003" {
		t.Fatalf("requests = %#v, want the one confirmed entry healed by the final heal", f.reacquirer.requests)
	}
}
