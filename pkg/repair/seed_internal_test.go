package repair

import (
	"errors"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
)

type journalReacquirer struct {
	fakeReacquirer

	jobs []reacquire.Job
}

func (j *journalReacquirer) Jobs() []reacquire.Job { return j.jobs }

func TestRestartSeedsTheBreakerFromTheLastHoursJobs(t *testing.T) {
	t.Parallel()
	service, clock := newAdmitService(t)
	now := clock.Now()
	var jobs []reacquire.Job
	for i := range 30 {
		jobs = append(jobs, reacquire.Job{
			ID: "recent", EntryID: "e" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Cause: reacquire.CauseRepair,
			Strategy: reacquire.StrategyCommandSearch, CreatedAt: now.Add(-10 * time.Minute),
		})
	}
	jobs = append(jobs,
		reacquire.Job{ID: "old", EntryID: "old", Cause: reacquire.CauseRepair, CreatedAt: now.Add(-3 * time.Hour)},
		reacquire.Job{
			ID: "grab", EntryID: "grab", Cause: reacquire.CauseRepair,
			Strategy: reacquire.StrategyDownloadFailed, CreatedAt: now.Add(-time.Minute),
		},
	)
	service.reacquirer = &journalReacquirer{jobs: jobs}

	service.seedBreakerFromJournal()
	service.seedBreakerFromJournal() // a config change calls Start again: no double count

	admitted := 0
	var capped error
	for range 40 {
		if err := service.Admit(t.Context(), HealRequest{Provider: unknownProvider, Class: ClassOther}); err != nil {
			capped = err
			break
		}
		admitted++
	}
	// 30 recent heals of 30 entries: the rolling window already shows a tripped bucket.
	if admitted != 0 || !errors.Is(capped, ErrHealHeld) {
		t.Fatalf(
			"admitted %d after a restart with 30 recent heals, last error %v; want the caps and window carried over",
			admitted,
			capped,
		)
	}
}
