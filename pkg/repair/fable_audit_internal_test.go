package repair

import (
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// SM12-44, Fable audit finding 1: held rows with an open reacquisition are still the backlog that tripped the
// bucket. On a closed bucket (fresh after a restart) they count, so 131 held rows, 82 of them in flight, among 1500
// clean probes trip it and none of them heals; the in-flight ones stay held.
func TestInFlightHeldRowsStillTripAClosedBucket(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	key := rdHoster()
	f.service.breaker.mu.Lock()
	f.service.breaker.verdicts[key] = canaryVerdict{outcome: canaryPass, at: f.clock.Now()}
	f.service.breaker.mu.Unlock()

	open := map[string]bool{}
	batch := newProbeBatch(true, open)
	batch.sweep = true
	var held []string
	for i := range 131 {
		name := fmt.Sprintf("held-%03d", i)
		hash := "hash-" + name
		open[hash] = i < 82
		file := storage.BrokenFile{
			EntryName: name, FileName: "video.mkv", InfoHash: hash, Provider: "rd", Reason: reasonHosterUnavailable,
		}
		health := &storage.EntryHealth{
			EntryName: name, Status: storage.HealthBroken, BrokenFiles: []storage.BrokenFile{file},
		}
		batch.noteProbed(&probeResult{health: health, providers: []string{"rd"}, wasHeld: true})
		batch.add(health, health.BrokenFiles, true)
		held = append(held, name)
	}
	for i := range 1500 {
		health := &storage.EntryHealth{EntryName: fmt.Sprintf("clean-%04d", i), Status: storage.HealthHealthy}
		batch.noteProbed(&probeResult{health: health, providers: []string{"rd"}})
	}

	run := &storage.RepairRun{ID: "sweep"}
	f.service.healBatch(t.Context(), run, batch, healPolicy{confirm: true, sweep: true})
	for name, entry := range batch.entries {
		if entry.admitted {
			t.Fatalf("%s admitted: in-flight held rows hid the backlog from a closed bucket", name)
		}
	}
	if run.Stats.Held != 131 {
		t.Fatalf("held = %d, want 131", run.Stats.Held)
	}
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != string(breakerTripped) {
		t.Fatalf("breaker = %+v, want rd/hoster_link tripped", status)
	}
	for _, name := range held {
		h, err := f.store.GetEntryHealth(name)
		if err != nil || h.Status != storage.HealthHeld || h.HeldBucket != key.String() {
			t.Fatalf("%s = %+v (%v), want held under %s", name, h, err, key)
		}
	}
}

// SM12-44: a needs_attention job waits for an operator, not for a heal, and a job open longer than the Window is
// stuck: neither makes its entry in flight, so neither hides it from the breaker.
func TestOpenReacquisitionsSkipsJobsWaitingOnAnOperatorOrStuck(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	now := f.clock.Now()
	f.service.reacquirer = &journalReacquirer{jobs: []reacquire.Job{
		{Status: reacquire.StatusQueued, EntryID: "fresh", CreatedAt: now.Add(-10 * time.Minute)},
		{Status: reacquire.StatusWaitingForImport, EntryID: "bound", CreatedAt: now.Add(-59 * time.Minute),
			Bindings: []reacquire.Binding{{EntryID: "bound-binding"}}},
		{Status: reacquire.StatusNeedsAttention, EntryID: "operator", CreatedAt: now},
		{Status: reacquire.StatusWaitingForDownload, EntryID: "stuck", CreatedAt: now.Add(-2 * time.Hour)},
		{Status: reacquire.StatusReady, EntryID: "done", CreatedAt: now},
	}}
	got := f.service.openReacquisitions()
	want := map[string]bool{"fresh": true, "bound": true, "bound-binding": true}
	if len(got) != len(want) {
		t.Fatalf("open = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("open = %v, want %v", got, want)
		}
	}
}

// SM12-45, Fable audit finding 2: a bucket the rolling window tripped (no sweep did) is not released by a sweep's
// clean sample while its held rows are over threshold: that sweep trips it, and only a later one may release it.
func TestWindowTrippedBucketIsNotReleasedOnTheSample(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, _ := testBreaker(&verdict)
	key := testBucket()
	b.mu.Lock()
	b.tripLocked(b.bucketLocked(key), clock.Now(), "", holdTripped)
	b.mu.Unlock()
	clock.Advance(2 * time.Hour)

	in := bucketInput{runID: "sweep-1", confirmed: 60, probed: 90, held: 60, sample: sweepSample{probed: 30}}
	if out := b.evaluate(t.Context(), key, in); out.released || out.hold == holdNone {
		t.Fatalf("sweep-1 = %+v, want held: the window trip is no sweep's", out)
	}
	clock.Advance(2 * time.Hour)
	in.runID = "sweep-2"
	if out := b.evaluate(t.Context(), key, in); !out.released {
		t.Fatalf("sweep-2 = %+v, want released on its clean sample after sweep-1 tripped the bucket", out)
	}
}

// SM12-46, Fable audit finding 3: a bucket admits an entry only if it fits under the cap, except the first, so a
// multi-file entry cannot carry the bucket past it.
func TestTryAdmitDoesNotOvershootTheBucketCap(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, _, _ := testBreaker(&verdict)
	key := testBucket()
	usage := newRunUsage()
	if !b.tryAdmit([]healItem{{bucket: key, units: 24}}, usage) {
		t.Fatal("24 units refused under a cap of 25")
	}
	if b.tryAdmit([]healItem{{bucket: key, units: 12}}, usage) {
		t.Fatal("12 more units admitted at 24 of 25")
	}
	if !b.tryAdmit([]healItem{{bucket: key, units: 1}}, usage) {
		t.Fatal("the 25th unit refused")
	}
	other := Bucket{Provider: "usenet", Class: ClassUsenetSegment}
	if !b.tryAdmit([]healItem{{bucket: other, units: 24}}, usage) {
		t.Fatal("24 units refused at a run total of 25 of 50")
	}
	third := Bucket{Provider: "ad", Class: ClassUsenetSegment}
	if b.tryAdmit([]healItem{{bucket: third, units: 2}}, newRunUsage()) {
		t.Fatal("2 more units admitted at a window total of 49 of 50")
	}

	fresh, _, _ := testBreaker(&verdict)
	if !fresh.tryAdmit([]healItem{{bucket: other, units: 30}}, newRunUsage()) {
		t.Fatal("an entry bigger than the cap refused on an empty bucket: it must still heal once")
	}
}

// SM12-47, Fable audit finding 4: with no canary, a releasing bucket drains only on a sweep sample big enough to
// judge; a smaller sample, or none (outside a sweep), holds the drain.
func TestReleasingBucketWithNoCanaryAndNoSampleHolds(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]bucketInput{
		"small sample":  {runID: "sweep", confirmed: 0, probed: 2, sample: sweepSample{probed: 2}},
		"outside sweep": {probed: -1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			verdict := canaryNone
			b, _, _ := testBreaker(&verdict)
			key := testBucket()
			b.mu.Lock()
			b.bucketLocked(key).state = breakerReleasing
			b.mu.Unlock()
			out := b.evaluate(t.Context(), key, in)
			if out.draining || out.hold == holdNone {
				t.Fatalf("out = %+v, want the drain held without evidence", out)
			}
		})
	}
	verdict := canaryNone
	b, _, _ := testBreaker(&verdict)
	key := testBucket()
	b.mu.Lock()
	b.bucketLocked(key).state = breakerReleasing
	b.mu.Unlock()
	in := bucketInput{runID: "sweep", probed: 3, sample: sweepSample{probed: 3}}
	if out := b.evaluate(t.Context(), key, in); !out.draining {
		t.Fatalf("out = %+v, want a clean sample of 3 to drain", out)
	}
}
