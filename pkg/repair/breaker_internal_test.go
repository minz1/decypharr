package repair

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func testBucket() Bucket { return Bucket{Provider: "realdebrid", Class: ClassHosterLink} }

// testBreaker builds a breaker with the default settings, a fake clock and a canary that answers verdict.
func testBreaker(verdict *canaryOutcome) (*breaker, *fakeClock, *int) {
	clock := newFakeClock()
	calls := 0
	b := newBreaker(
		func() config.BreakerSettings { return config.RepairConfig{}.BreakerSettings() },
		func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
			calls++
			return *verdict
		},
	)
	b.now = clock.Now
	return b, clock, &calls
}

func TestBreakerTransitionTableAcceptsExactlyTheLegalEdges(t *testing.T) {
	t.Parallel()
	states := []breakerState{breakerClosed, breakerTripped, breakerReleasing}
	legal := map[[2]breakerState]bool{
		{breakerClosed, breakerTripped}:    true,
		{breakerTripped, breakerTripped}:   true,
		{breakerTripped, breakerReleasing}: true,
		{breakerReleasing, breakerTripped}: true,
		{breakerReleasing, breakerClosed}:  true,
	}
	at := newFakeClock().Now()
	for _, from := range states {
		for _, to := range states {
			bk := &bucketBreaker{state: from}
			err := bk.transition(to, at, "run", holdTripped)
			switch {
			case legal[[2]breakerState{from, to}] && err != nil:
				t.Errorf("%s -> %s refused: %v", from, to, err)
			case legal[[2]breakerState{from, to}] && bk.state != to:
				t.Errorf("%s -> %s left the bucket in %s", from, to, bk.state)
			case !legal[[2]breakerState{from, to}] && !errors.Is(err, errBreakerEdge):
				t.Errorf("%s -> %s = %v, want the illegal-edge error", from, to, err)
			case !legal[[2]breakerState{from, to}] && bk.state != from:
				t.Errorf("a refused %s -> %s moved the bucket to %s", from, to, bk.state)
			}
		}
	}
	for _, from := range states {
		for _, to := range from.successors() {
			if !legal[[2]breakerState{from, to}] {
				t.Errorf("successors() lists %s -> %s, which the table does not allow", from, to)
			}
		}
	}
}

func TestOverThresholdRule(t *testing.T) {
	t.Parallel()
	s := config.RepairConfig{}.BreakerSettings()
	for _, tc := range []struct {
		confirmed, probed int
		want              bool
	}{
		{9, 20, false},    // under the minimum however large the share
		{10, 4000, false}, // 0.25 percent of a big sweep
		{10, 200, true},   // exactly 5 percent
		{10, 201, false},
		{12, 300, false},
		{50, 100000, true}, // the absolute count
		{49, 100000, false},
		{81, 4065, true}, // the 2026-10-12 incident
		{10, 0, true},
		{10, -1, true}, // no scale: the count alone
		{9, -1, false},
	} {
		if got := over(s, tc.confirmed, tc.probed); got != tc.want {
			t.Errorf("over(confirmed=%d, probed=%d) = %v, want %v", tc.confirmed, tc.probed, got, tc.want)
		}
	}
}

func TestEvaluateTripsAClosedBucketOnceAndHoldsIt(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, _, canaryCalls := testBreaker(&verdict)
	out := b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r1", confirmed: 81, probed: 4065})
	if out.hold != holdTripped || !out.newlyTripped {
		t.Fatalf("outcome = %+v, want a new trip", out)
	}
	if *canaryCalls != 0 {
		t.Fatalf("canary ran %d times for a bucket already over threshold", *canaryCalls)
	}
	again := b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r1", confirmed: 81, probed: 4065})
	if again.hold != holdTripped || again.newlyTripped {
		t.Fatalf("second outcome = %+v, want held without a second notification", again)
	}
}

func TestEvaluateReleasesOnlyInALaterRunAfterTheHoldAndAPassingCanary(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, canaryCalls := testBreaker(&verdict)
	b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r1", confirmed: 81, probed: 4065})
	under := func(run string) bucketOutcome {
		return b.evaluate(t.Context(), testBucket(), bucketInput{runID: run, confirmed: 2, probed: 4000})
	}

	if out := under("r2"); out.hold != holdTripped || out.released {
		t.Fatalf("a run right after the trip released it: %+v", out)
	}
	clock.Advance(time.Hour)
	if out := under("r1"); out.hold != holdTripped || out.released {
		t.Fatalf("the tripping run itself released the bucket: %+v", out)
	}
	if *canaryCalls != 0 {
		t.Fatalf("canary ran %d times before a release was possible", *canaryCalls)
	}
	out := under("r2")
	if out.hold != holdNone || !out.released || *canaryCalls != 1 {
		t.Fatalf("outcome = %+v canary calls %d, want released after one canary", out, *canaryCalls)
	}
	if state := b.buckets[testBucket()].state; state != breakerReleasing {
		t.Fatalf("state = %s, want releasing", state)
	}
	b.settle(testBucket())
	if state := b.buckets[testBucket()].state; state != breakerClosed {
		t.Fatalf("state after settle = %s, want closed", state)
	}
}

// SM12-20: held rows still broken count on a closed bucket (a restart forgot the trip), and on an open bucket only
// when no clean sample shows the provider working.
func TestHeldRowsCountOnlyWithoutACleanSampleOnAnOpenBucket(t *testing.T) {
	t.Parallel()
	verdict := canaryNone
	b, clock, _ := testBreaker(&verdict)
	clean := sweepSample{probed: 20}
	heldOnly := bucketInput{runID: "r1", confirmed: 80, held: 80, probed: 100, sample: clean}
	if out := b.evaluate(t.Context(), testBucket(), heldOnly); out.hold != holdTripped || !out.newlyTripped {
		t.Fatalf("closed bucket: outcome = %+v, want the held rows to trip it", out)
	}
	clock.Advance(2 * time.Hour)
	dirty := heldOnly
	dirty.runID, dirty.sample = "r2", sweepSample{probed: 20, broken: 15}
	if out := b.evaluate(t.Context(), testBucket(), dirty); out.hold != holdTripped || out.released {
		t.Fatalf("dirty sample: outcome = %+v, want still tripped", out)
	}
	clock.Advance(2 * time.Hour)
	// A clean sample is judged on its own scale: 15 of 200 is over 5%, though 15 of all 400 probed is not.
	shareOfSample := bucketInput{
		runID: "r3", confirmed: 95, held: 80, probed: 400, sample: sweepSample{probed: 200, broken: 15},
	}
	if out := b.evaluate(t.Context(), testBucket(), shareOfSample); out.hold != holdTripped || out.released {
		t.Fatalf("over 5%% of the sample: outcome = %+v, want still tripped", out)
	}
	clock.Advance(2 * time.Hour)
	later := heldOnly
	later.runID = "r4"
	if out := b.evaluate(t.Context(), testBucket(), later); !out.released {
		t.Fatalf("clean sample: outcome = %+v, want released despite 80 held rows still broken", out)
	}
}

func TestEvaluateStaysTrippedWhileStillOverThreshold(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, _ := testBreaker(&verdict)
	b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r1", confirmed: 30, probed: 100})
	clock.Advance(3 * time.Hour)
	out := b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r2", confirmed: 30, probed: 100})
	if out.hold != holdTripped || out.newlyTripped || out.released {
		t.Fatalf("outcome = %+v, want still held", out)
	}
	if got := b.buckets[testBucket()].since; !got.Equal(clock.Now()) {
		t.Fatalf("a fresh over-threshold run did not restart the release clock: since = %v", got)
	}
}

func TestCanaryDecidesAClosedBucketUnderThreshold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		verdict canaryOutcome
		hold    holdReason
		state   breakerState
	}{
		{"pass", canaryPass, holdNone, breakerClosed},
		{"no signal", canaryNone, holdNone, breakerClosed},
		{"same failure trips", canaryFail, holdCanaryFailed, breakerTripped},
		{"inconclusive holds this run only", canaryInconclusive, holdCanaryInconclusive, breakerClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict := tc.verdict
			b, _, _ := testBreaker(&verdict)
			out := b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r", confirmed: 1, probed: 100})
			if out.hold != tc.hold {
				t.Errorf("hold = %q, want %q", out.hold, tc.hold)
			}
			if state := b.bucketLocked(testBucket()).state; state != tc.state {
				t.Errorf("state = %s, want %s", state, tc.state)
			}
		})
	}
}

func TestCanaryVerdictIsReusedWithinItsCacheAndRefreshedAfter(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, calls := testBreaker(&verdict)
	in := bucketInput{confirmed: 1, probed: -1}
	b.evaluate(t.Context(), testBucket(), in)
	clock.Advance(14 * time.Minute)
	b.evaluate(t.Context(), testBucket(), in)
	if *calls != 1 {
		t.Fatalf("canary ran %d times inside its cache, want 1", *calls)
	}
	clock.Advance(2 * time.Minute)
	b.evaluate(t.Context(), testBucket(), in)
	if *calls != 2 {
		t.Fatalf("canary ran %d times after its cache expired, want 2", *calls)
	}
}

func TestWindowTripCountsDistinctSubjectsInTheRollingHour(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, _ := testBreaker(&verdict)
	for i := range 9 {
		b.noteConfirmed(testBucket(), fmt.Sprintf("entry-%d", i))
	}
	b.noteConfirmed(testBucket(), "entry-0") // the same subject twice is one
	if out := b.evaluate(t.Context(), testBucket(), bucketInput{confirmed: 0, probed: -1}); out.hold != holdNone {
		t.Fatalf("nine distinct confirmations tripped the bucket: %+v", out)
	}
	b.noteConfirmed(testBucket(), "entry-9")
	if out := b.evaluate(t.Context(), testBucket(), bucketInput{confirmed: 0, probed: -1}); out.hold != holdTripped {
		t.Fatalf("ten distinct confirmations did not trip the bucket: %+v", out)
	}
	clock.Advance(61 * time.Minute)
	if got := b.windowCount(testBucket()); got != 0 {
		t.Fatalf("window count after an hour = %d, want the old confirmations aged out", got)
	}
}

func TestCapsLimitBucketAndTotalPerRunAndPerWindow(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, _ := testBreaker(&verdict)
	other := Bucket{Provider: "torbox", Class: ClassHosterLink}
	third := Bucket{Provider: "usenet", Class: ClassUsenetSegment}
	usage := newRunUsage()

	admitted := map[Bucket]int{}
	for _, key := range []Bucket{testBucket(), other} {
		for range 30 {
			if b.tryAdmit([]healItem{{bucket: key, units: 1}}, usage) {
				admitted[key]++
			}
		}
	}
	if admitted[testBucket()] != 25 || admitted[other] != 25 {
		t.Fatalf("admitted %v, want 25 per bucket", admitted)
	}
	if b.tryAdmit([]healItem{{bucket: third, units: 1}}, usage) {
		t.Fatal("a third bucket was admitted past the 50 total cap")
	}
	// The window caps a second run that starts straight after.
	if b.tryAdmit([]healItem{{bucket: testBucket(), units: 1}}, newRunUsage()) {
		t.Fatal("a new run exceeded the rolling-hour cap")
	}
	clock.Advance(61 * time.Minute)
	if !b.tryAdmit([]healItem{{bucket: testBucket(), units: 1}}, newRunUsage()) {
		t.Fatal("capacity did not come back after the window")
	}
}

func TestAnEntryBiggerThanTheCapStillHealsOnce(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, _, _ := testBreaker(&verdict)
	usage := newRunUsage()
	if !b.tryAdmit([]healItem{{bucket: testBucket(), units: 40}}, usage) {
		t.Fatal("a 40-file entry was never admitted, so it could never heal")
	}
	if b.tryAdmit([]healItem{{bucket: testBucket(), units: 1}}, usage) {
		t.Fatal("the bucket kept admitting after its cap was passed")
	}
}

func TestForcedHealsCountTowardTheCaps(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, _, _ := testBreaker(&verdict)
	b.recordForced([]healItem{{bucket: testBucket(), units: 25}}, nil)
	if b.tryAdmit([]healItem{{bucket: testBucket(), units: 1}}, nil) {
		t.Fatal("heals an operator forced did not count toward the bucket cap")
	}
}

func TestStatusListsOnlyOpenBuckets(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, _, _ := testBreaker(&verdict)
	b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r", confirmed: 60, probed: 100})
	b.evaluate(
		t.Context(),
		Bucket{Provider: "torbox", Class: ClassOther},
		bucketInput{runID: "r", confirmed: 0, probed: 100},
	)
	got := b.status()
	if len(got) != 1 || got[0].Provider != "realdebrid" || got[0].State != "tripped" {
		t.Fatalf("status = %+v, want only the tripped bucket", got)
	}
}

func TestClassForReason(t *testing.T) {
	t.Parallel()
	for reason, want := range map[string]Class{
		"hoster_unavailable":      ClassHosterLink,
		"empty_download_link":     ClassHosterLink,
		"usenet_segment_missing":  ClassUsenetSegment,
		"usenet_corrupt_content":  ClassUsenetManifest,
		"usenet_manifest_missing": ClassUsenetManifest,
		"usenet_manifest_invalid": ClassUsenetManifest,
		"torrent_not_found":       ClassDropped,
		"placement_not_found":     ClassDropped,
		"missing_provider_link":   ClassOther,
		"":                        ClassOther,
	} {
		if got := ClassForReason(reason); got != want {
			t.Errorf("ClassForReason(%q) = %q, want %q", reason, got, want)
		}
	}
}
