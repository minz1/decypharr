package manager

import (
	"testing"

	"github.com/go-co-op/gocron/v2"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestSchedulePruneRegistersJobOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{true, false} {
		h := newPruneHarness(t, config.PruneModeReport)
		h.setPrune(t, func(p *config.Prune) { p.Enabled = enabled })
		scheduler, err := gocron.NewScheduler()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = scheduler.Shutdown() })
		h.m.scheduler = scheduler

		h.m.schedulePrune(t.Context())

		got := false
		for _, j := range scheduler.Jobs() {
			got = got || j.Name() == "prune"
		}
		if got != enabled {
			t.Fatalf("enabled=%v registered=%v", enabled, got)
		}
	}
}
