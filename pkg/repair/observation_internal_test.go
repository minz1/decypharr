package repair

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A second observation confirms a broken file only from a different run and at least the gap after the first.

func TestSecondObservationFromTheSameRunDoesNotConfirm(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	f.probe(t.Context(), t, true)
	run := f.run.ID
	f.clock.Advance(2 * time.Hour)
	candidates := f.candidates()
	if err := f.service.probeAndHealCandidates(
		t.Context(),
		&storage.RepairRun{ID: run},
		candidates,
		[]string{f.entry.Name},
		newErrorCache(),
		RunOptions{},
		true,
		healPolicy{confirm: true},
	); err != nil {
		t.Fatal(err)
	}
	if len(f.reacquirer.requests) != 0 {
		t.Fatalf("a probe inside the run that first saw the failure confirmed it: %#v", f.reacquirer.requests)
	}
}

func TestSecondObservationInsideTheGapDoesNotConfirm(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	f.probe(t.Context(), t, true)
	f.nextRunWithin(30 * time.Minute)
	f.probeAgain(t)
	if len(f.reacquirer.requests) != 0 {
		t.Fatalf("a second run 30 minutes later confirmed the failure: %#v", f.reacquirer.requests)
	}
	// The unconfirmed re-probe must not restart the clock: two hours after the FIRST sight it confirms.
	f.nextRunWithin(90 * time.Minute)
	f.probeAgain(t)
	if len(f.reacquirer.requests) != 1 {
		t.Fatalf("requests = %d, want 1 once the gap since the first observation passed", len(f.reacquirer.requests))
	}
}

func TestBrokenRowWithoutFirstSeenCountsAsAFirstObservation(t *testing.T) {
	t.Parallel()
	f := newSweepFixture(t, "video.mkv")
	if err := f.store.SaveEntryHealth(&storage.EntryHealth{
		EntryName: f.entry.Name, Status: storage.HealthBroken,
		BrokenFiles: []storage.BrokenFile{{FileName: "video.mkv", InfoHash: sweepEntryHash}},
	}); err != nil {
		t.Fatal(err)
	}
	f.probe(t.Context(), t, true)
	if len(f.reacquirer.requests) != 0 {
		t.Fatalf("a row written before first-seen existed confirmed on one probe: %#v", f.reacquirer.requests)
	}
	saved, _ := f.store.GetEntryHealth(f.entry.Name)
	if saved == nil || len(saved.BrokenFiles) != 1 || saved.BrokenFiles[0].FirstSeenAt.IsZero() ||
		saved.BrokenFiles[0].FirstSeenRunID != f.run.ID {
		t.Fatalf("saved health = %#v, want this probe recorded as the first observation", saved)
	}
	f.probe(t.Context(), t, true)
	if len(f.reacquirer.requests) != 1 {
		t.Fatalf("requests = %d, want 1 on the independent second observation", len(f.reacquirer.requests))
	}
}

// nextRunWithin starts a new run the given time after the previous one.
func (f *sweepFixture) nextRunWithin(after time.Duration) {
	f.probes++
	f.clock.Advance(after)
	f.run = &storage.RepairRun{ID: "run-x" + time.Duration(f.probes).String()}
}

// probeAgain probes the entry in the current run without starting a new one.
func (f *sweepFixture) probeAgain(t *testing.T) {
	t.Helper()
	if err := f.service.probeAndHealCandidates(
		t.Context(),
		f.run,
		f.candidates(),
		[]string{f.entry.Name},
		newErrorCache(),
		RunOptions{},
		true,
		healPolicy{confirm: true},
	); err != nil {
		t.Fatal(err)
	}
}
