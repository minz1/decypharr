package repair

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// An interval schedule counted from process start never fired on days with several restarts.
func TestSweepStartAnchorsIntervalToLastSweep(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 23, 0, 0, 0, time.UTC)
	run := func(age time.Duration, reason string) *storage.RepairRun {
		return &storage.RepairRun{
			StartedAt: now.Add(-age), Status: storage.RepairRunCompleted, CancelReason: reason,
			Trigger: storage.RepairTriggerScheduled,
		}
	}
	manual := run(time.Hour, "")
	manual.Trigger = storage.RepairTriggerManual
	for name, tc := range map[string]struct {
		schedule string
		runs     []*storage.RepairRun
		want     time.Time
		anchored bool
	}{
		"next due later":            {"24h", []*storage.RepairRun{run(10*time.Hour, "")}, now.Add(14 * time.Hour), true},
		"overdue":                   {"24h", []*storage.RepairRun{run(40*time.Hour, "")}, now.Add(sweepStartupGrace), true},
		"interrupted run":           {"24h", []*storage.RepairRun{run(time.Hour, interruptedByRestart), run(40*time.Hour, "")}, now.Add(sweepStartupGrace), true},
		"manual run does not count": {"24h", []*storage.RepairRun{manual, run(40*time.Hour, "")}, now.Add(sweepStartupGrace), true},
		"no sweep yet":              {"24h", nil, time.Time{}, false},
		"wall-clock schedule":       {"04:30", []*storage.RepairRun{run(time.Hour, "")}, time.Time{}, false},
		"cron schedule":             {"0 4 * * *", []*storage.RepairRun{run(time.Hour, "")}, time.Time{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, anchored := sweepStartAt(tc.schedule, tc.runs, now)
			if anchored != tc.anchored || !got.Equal(tc.want) {
				t.Fatalf("sweepStartAt() = %s, %v; want %s, %v", got, anchored, tc.want, tc.anchored)
			}
		})
	}
}
