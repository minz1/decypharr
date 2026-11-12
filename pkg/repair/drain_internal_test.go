package repair

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
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
	for i := range 10 {
		probe(fmt.Sprintf("held-%03d", i), true, true, false)
	}
	for i := range 30 {
		probe(fmt.Sprintf("clean-%03d", i), false, false, false)
	}
	for i := range 10 {
		probe(fmt.Sprintf("broken-%03d", i), true, false, false)
	}

	plan := f.service.judgeBuckets(t.Context(), "sweep", batch, batch.pendingNames())
	if plan[key].hold == holdNone {
		t.Fatalf("plan = %+v: 10 newly broken of 40 real probes must re-trip the releasing bucket", plan[key])
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
