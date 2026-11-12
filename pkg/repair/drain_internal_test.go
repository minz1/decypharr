package repair

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// SM12-25: an Admit that releases a tripped bucket between sweeps must leave it releasing while held rows name it,
// so the next sweep drains them instead of counting them on a closed bucket and re-tripping.
func TestAdmitReleaseBetweenSweepsLeavesTheDrainToTheSweep(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.service.reacquirer = openJobsReacquirer{f.reacquirer}
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)
	if f.countStatus(t, storage.HealthHeld) != 80 {
		t.Fatalf("setup: held %d, want 80", f.countStatus(t, storage.HealthHeld))
	}

	f.clock.Advance(2 * time.Hour) // past ReleaseAfter; entries 80.. are recently good, so the canary passes
	err := f.service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "other"})
	if err != nil && !errors.Is(err, ErrHealCapped) {
		t.Fatalf("admit: %v, want the bucket released", err)
	}
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerReleasing) {
		t.Fatalf("after the admit: %+v, want releasing while 80 held rows name the bucket", status)
	}

	f.sweep(t.Context(), nil)
	assertNotTripped(t, f, "sweep after the admit release")
	if got := f.requests(); got < 24 {
		t.Fatalf("heals = %d, want the sweep to drain the held entries", got)
	}
}

// SM12-26: a stop-schedule partial sweep re-probes only some held entries; the rest are still held, so the bucket
// stays releasing and the next sweep keeps draining.
func TestStopSchedulePartialSweepKeepsTheDrainOpen(t *testing.T) {
	t.Parallel()
	f := startDrain(t)

	ctx, stop := f.stopAfterChecks(t, 10)
	f.sweep(ctx, stop)
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerReleasing) {
		t.Fatalf("after the stopped sweep: %+v (held %d), want releasing", status, f.countStatus(t, storage.HealthHeld))
	}
	before := f.requests()
	f.sweep(t.Context(), nil)
	assertNotTripped(t, f, "sweep after the stopped sweep")
	if f.requests() <= before {
		t.Fatalf("heals = %d, want more than %d: the drain goes on", f.requests(), before)
	}
}

// SM12-27: entries with an open reacquisition are no evidence, so they leave the scale of a releasing bucket too:
// 10 newly broken among 40 real probes re-trip it however many in-flight entries the sweep also probed.
func TestInFlightEntriesDoNotDiluteAReleasingBucket(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	key := rdHoster()
	f.service.breaker.mu.Lock()
	f.service.breaker.bucketLocked(key).state = breakerReleasing
	f.service.breaker.mu.Unlock()

	batch := inFlightBatch(10)
	plan := f.service.judgeBuckets(t.Context(), "sweep", batch, batch.pendingNames())
	if plan[key].hold == holdNone {
		t.Fatalf("plan = %+v: 10 newly broken of 40 real probes must re-trip the releasing bucket", plan[key])
	}
}

// SM12-37: in-flight entries leave the scale of every scaled judgement, so they cannot dilute a closed bucket either,
// even one whose cached canary passes.
func TestInFlightEntriesDoNotDiluteAClosedBucket(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	key := rdHoster()
	f.service.breaker.mu.Lock()
	f.service.breaker.verdicts[key] = canaryVerdict{outcome: canaryPass, at: f.clock.Now()}
	f.service.breaker.mu.Unlock()

	batch := inFlightBatch(0)
	plan := f.service.judgeBuckets(t.Context(), "sweep", batch, batch.pendingNames())
	if plan[key].hold == holdNone || !plan[key].newlyTripped {
		t.Fatalf("plan = %+v: 10 newly broken of 40 real probes must trip the closed bucket", plan[key])
	}
}

// inFlightBatch is a sweep batch on rd: 250 broken entries with an open reacquisition, held broken entries, 30
// clean and 10 newly broken.
func inFlightBatch(held int) *probeBatch {
	open := map[string]bool{}
	batch := newProbeBatch(true, open)
	batch.sweep = true
	probe := func(name string, broken, held, inFlight bool) {
		health := &storage.EntryHealth{EntryName: name, Status: storage.HealthHealthy}
		if broken {
			hash := fmt.Sprintf("hash-%s", name)
			health.Status = storage.HealthBroken
			health.BrokenFiles = []storage.BrokenFile{{
				EntryName: name, FileName: "video.mkv", InfoHash: hash, Provider: "rd", Reason: reasonHosterUnavailable,
			}}
			open[hash] = inFlight
		}
		batch.noteProbed(&probeResult{health: health, providers: []string{"rd"}, wasHeld: held})
		batch.add(health, health.BrokenFiles, held)
	}
	for i := range 250 {
		probe(fmt.Sprintf("inflight-%03d", i), true, false, true)
	}
	for i := range held {
		probe(fmt.Sprintf("held-%03d", i), true, true, false)
	}
	for i := range 30 {
		probe(fmt.Sprintf("clean-%03d", i), false, false, false)
	}
	for i := range 10 {
		probe(fmt.Sprintf("broken-%03d", i), true, false, false)
	}
	return batch
}

// SM12-36, R0.1 run 3: a drain lives over several sweeps, so each drain sweep runs the canary as any other
// evaluation does. A provider-wide outage mid-drain under MinConfirmed trips the bucket on the canary and heals none.
func TestDrainSweepRunsTheCanary(t *testing.T) {
	t.Parallel()
	f := startDrain(t) // 25 healed, 55 held
	f.markUpDirty(t)
	f.sweep(t.Context(), nil) // the drain goes on: 50 healed, 30 held
	assertNotTripped(t, f, "drain sweep")

	f.client.set(true) // the whole provider goes down
	before := f.requests()
	for sweep := range 2 { // three entries that are not held: first sighting, then confirmed
		for i := 80; i < 83; i++ {
			err := f.store.MarkEntryDirty(f.entries[i].Name, config.ProtocolTorrent, "entry_item_changed")
			if err != nil {
				t.Fatal(err)
			}
		}
		f.sweep(t.Context(), nil)
		if sweep == 0 {
			if status := f.service.BreakerStatus(); len(status) != 1 || status[0].Reason != string(holdCanaryFailed) {
				t.Fatalf("first outage sweep: %+v, want the canary to trip the draining bucket", status)
			}
		}
	}
	if got := f.requests(); got != before {
		t.Fatalf("heals = %d, want %d: a drain sweep healed under a failing canary", got, before)
	}
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerTripped) {
		t.Fatalf("after the outage sweeps: %+v, want tripped", status)
	}
}

// Item 59 review: an inconclusive canary on a drain sweep heals nothing and closes nothing: the bucket stays
// releasing and its rows stay held for the next passing canary.
func TestInconclusiveCanaryPausesTheDrain(t *testing.T) {
	t.Parallel()
	f := startDrain(t) // 25 healed, 55 held
	b := f.service.breaker
	b.mu.Lock()
	// Stamped at the next sweep's time (sweep advances the clock 25h), so the sweep finds it fresh.
	b.verdicts[rdHoster()] = canaryVerdict{outcome: canaryInconclusive, at: f.clock.Now().Add(25 * time.Hour)}
	b.mu.Unlock()

	f.sweep(t.Context(), nil)
	if got := f.requests(); got != 25 {
		t.Fatalf("heals = %d, want 25: an inconclusive canary healed held entries", got)
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 55 {
		t.Fatalf("held = %d, want 55", got)
	}
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerReleasing) {
		t.Fatalf("after the sweep: %+v, want releasing", status)
	}
}

// SM12-38: an Admit whose canary passes on a releasing bucket that no held row names closes it, as a release does.
func TestAdmitOnADrainedReleasingBucketClosesIt(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	b := f.service.breaker
	b.mu.Lock()
	b.bucketLocked(rdHoster()).state = breakerReleasing
	b.verdicts[rdHoster()] = canaryVerdict{outcome: canaryPass, at: f.clock.Now()}
	b.mu.Unlock()

	err := f.service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "x"})
	if err != nil {
		t.Fatalf("admit: %v, want admitted", err)
	}
	if status := f.service.BreakerStatus(); len(status) != 0 {
		t.Fatalf("after the admit: %+v, want closed: no held row is left to drain", status)
	}
}

// Item 57 review: a held row no sweep can re-probe (its media left the Arr) must not keep a drained bucket
// releasing, where no canary runs: a complete sweep ends the drain on what it probed.
func TestOrphanHeldRowDoesNotKeepTheDrainOpen(t *testing.T) {
	t.Parallel()
	f := startDrain(t)
	orphan := &storage.EntryHealth{
		EntryName: "gone", Status: storage.HealthHeld, HeldBucket: rdHoster().String(), HeldAt: f.clock.Now(),
	}
	if err := f.store.SaveEntryHealth(orphan); err != nil {
		t.Fatal(err)
	}
	for sweep := range 3 {
		f.sweep(t.Context(), nil)
		assertNotTripped(t, f, fmt.Sprintf("drain sweep %d", sweep))
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 0 {
		t.Fatalf("still held = %d after the drain", got)
	}
	if status := f.service.BreakerStatus(); len(status) != 0 {
		t.Fatalf("an orphan held row kept the bucket open: %+v", status)
	}
}

// SM12-31: a release outside a sweep is not a free pass. A recheck that releases the bucket closes it at run end when
// no held row names it, and while held rows keep it releasing, a heal outside a sweep still goes through the canary.
func TestHealOutsideASweepAfterARecheckReleaseRunsTheCanary(t *testing.T) {
	t.Parallel()
	t.Run("held rows keep it releasing", func(t *testing.T) {
		t.Parallel()
		f := newOutageFixture(t, 100, config.RepairConfig{})
		f.downRange(0, 80)
		f.sweep(t.Context(), nil)
		f.sweep(t.Context(), nil)
		f.clock.Advance(2 * time.Hour) // past ReleaseAfter; entries 80.. are recently good, so the canary passes
		recheck(t, f, 79)
		if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerReleasing) {
			t.Fatalf("after the recheck: %+v, want releasing while 79 held rows name the bucket", status)
		}
		// A recheck-fix is a heal outside a sweep too: the failing canary holds it, nothing is deleted.
		f.client.set(true)
		f.clock.Advance(2 * time.Hour) // the canary cache is over
		before := f.requests()
		recheck(t, f, 85)
		recheck(t, f, 85) // the second sighting confirms it
		if got := f.requests(); got != before {
			t.Fatalf("heals = %d, want %d: a recheck on a releasing bucket with a failing canary healed", got, before)
		}
		// So is an operator's fix of a broken row the cap or a restart left unheld.
		row := f.health(t, 85)
		row.Status, row.HeldBucket = storage.HealthBroken, ""
		if err := f.store.SaveEntryHealth(row); err != nil {
			t.Fatal(err)
		}
		before = f.requests()
		if _, err := f.service.FixBroken(t.Context(), []string{f.entries[85].Name}, FixOptions{}); err != nil {
			t.Fatal(err)
		}
		f.service.runWG.Wait()
		if got := f.requests(); got != before {
			t.Fatalf("heals = %d, want %d: a fix on a releasing bucket with a failing canary healed", got, before)
		}
		if got := f.health(t, 85).Status; got != storage.HealthHeld {
			t.Fatalf("status = %s, want held by the failing canary", got)
		}
		assertAdmitHeldByAFailingCanary(t, f)
	})
	t.Run("no held row left closes it", func(t *testing.T) {
		t.Parallel()
		f := newOutageFixture(t, 100, config.RepairConfig{})
		f.sweep(t.Context(), nil) // every entry good: canary candidates
		f.downRange(0, 1)
		held := f.health(t, 0)
		held.Status, held.HeldBucket, held.HeldAt = storage.HealthHeld, rdHoster().String(), f.clock.Now()
		held.BrokenFiles = []storage.BrokenFile{{
			EntryName: held.EntryName, FileName: "video.mkv", InfoHash: outageHash(0), Provider: "rd",
			Reason: reasonHosterUnavailable,
		}}
		if err := f.store.SaveEntryHealth(held); err != nil {
			t.Fatal(err)
		}
		f.service.breaker.mu.Lock()
		bk := f.service.breaker.bucketLocked(rdHoster())
		bk.state, bk.since, bk.reason = breakerTripped, f.clock.Now(), holdTripped
		f.service.breaker.mu.Unlock()
		f.clock.Advance(2 * time.Hour)
		recheck(t, f, 0)
		if got := f.requests(); got != 1 {
			t.Fatalf("heals = %d, want the rechecked entry healed by the release", got)
		}
		if status := f.service.BreakerStatus(); len(status) != 0 {
			t.Fatalf("after the recheck: %+v, want closed: no held row is left to drain", status)
		}
		assertAdmitHeldByAFailingCanary(t, f)
	})
}

func recheck(t *testing.T, f *outageFixture, i int) {
	t.Helper()
	if _, err := f.service.RecheckEntry(t.Context(), f.entries[i].Name, RecheckOptions{Fix: true}); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
}

// assertAdmitHeldByAFailingCanary breaks the whole provider and asks for a heal once the canary verdict is stale.
func assertAdmitHeldByAFailingCanary(t *testing.T, f *outageFixture) {
	t.Helper()
	f.client.set(true)
	f.clock.Advance(2 * time.Hour) // the canary cache and the heal window are over
	err := f.service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "other"})
	if !errors.Is(err, ErrHealHeld) {
		t.Fatalf("admit with a failing canary: %v, want held", err)
	}
}

// SM12-32: an Admit that releases the bucket after the sweep's plan held its entries must not let the sweep's end
// close it: the held rows the plan just saved still wait in it.
func TestReleaseAfterTheSweepPlanHeldTheBucketKeepsItReleasing(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)

	batch := newProbeBatch(true, nil)
	batch.sweep = true
	for i := range 80 {
		health := f.health(t, i)
		batch.noteProbed(&probeResult{health: health, providers: []string{"rd"}, wasHeld: true})
		batch.add(health, health.BrokenFiles, true)
	}
	// One entry of another provider heals after the plan; its heal is where the concurrent Admit release lands.
	other := f.health(t, 99)
	other.Status = storage.HealthBroken
	file := f.health(t, 0).BrokenFiles[0] // an Arr-known file, so the heal reaches the reacquirer
	file.EntryName, file.Provider = other.EntryName, "other"
	other.BrokenFiles = []storage.BrokenFile{file}
	batch.noteProbed(&probeResult{health: other, providers: []string{"other"}})
	batch.add(other, other.BrokenFiles, false)
	f.reacquirer.reacquire = func(reacquire.Request) (*reacquire.Job, error) {
		f.service.breaker.mu.Lock()
		f.service.breaker.bucketLocked(rdHoster()).state = breakerReleasing
		f.service.breaker.mu.Unlock()
		return &reacquire.Job{ID: "job"}, nil
	}

	f.service.healBatch(t.Context(), &storage.RepairRun{ID: "sweep-x"}, batch, healPolicy{confirm: true, sweep: true})
	if f.requests() != 1 {
		t.Fatalf("setup: heals = %d, want the other provider's entry", f.requests())
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 80 {
		t.Fatalf("setup: held = %d, want 80", got)
	}
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerReleasing) {
		t.Fatalf("after the sweep: %+v, want releasing: 80 held rows still name the bucket", status)
	}
}

// SM12-33: the drain holds only entries that were held. A new failure the cap leaves in a draining bucket stays
// broken, so the next sweep counts it as evidence instead of hiding it in the backlog.
func TestCappedNewFailureInADrainingBucketStaysBroken(t *testing.T) {
	t.Parallel()
	f := startDrain(t) // 25 healed, 55 held
	down := make([]string, 0, 81)
	for i := range 80 {
		down = append(down, outageHash(i))
	}
	f.client.set(false, append(down, outageHash(85))...)
	for range 2 { // first sighting, then confirmed
		if err := f.store.MarkEntryDirty(f.entries[85].Name, config.ProtocolTorrent, "entry_item_changed"); err != nil {
			t.Fatal(err)
		}
		f.sweep(t.Context(), nil)
	}
	assertNotTripped(t, f, "drain sweeps")
	if got := f.countStatus(t, storage.HealthHeld); got != 5 {
		t.Fatalf("held = %d, want the 5 old held entries the cap left", got)
	}
	if got := f.health(t, 85).Status; got != storage.HealthBroken {
		t.Fatalf("capped new failure saved %s, want broken", got)
	}
}

// SM12-34: a held-row scan that fails says nothing about the held rows, so a release outside a sweep must not close
// the bucket on it.
func TestHeldRowScanErrorKeepsTheBucketReleasing(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	b := f.service.breaker
	b.mu.Lock()
	bk := b.bucketLocked(rdHoster())
	bk.state, bk.since, bk.reason = breakerTripped, f.clock.Now(), holdTripped
	f.clock.Advance(2 * time.Hour)
	b.verdicts[rdHoster()] = canaryVerdict{outcome: canaryPass, at: f.clock.Now()}
	b.mu.Unlock()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}

	err := f.service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "x"})
	if err != nil {
		t.Fatalf("admit: %v, want the release to admit it", err)
	}
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerReleasing) {
		t.Fatalf("after a failed scan: %+v, want releasing", status)
	}
}
