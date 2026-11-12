package repair

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// trippedWithoutCanary trips testBucket and returns the breaker with a clock two hours past the trip, its canary
// finding nothing recently good to probe.
func trippedWithoutCanary(t *testing.T) (*breaker, *fakeClock) {
	t.Helper()
	verdict := canaryNone
	b, clock, _ := testBreaker(&verdict)
	b.evaluate(t.Context(), testBucket(), bucketInput{runID: "r1", confirmed: 81, probed: 4065})
	clock.Advance(2 * time.Hour)
	return b, clock
}

// With no canary candidate the clock alone never releases a tripped bucket.
func TestTrippedBucketWithoutACanaryStaysTrippedPastTheHold(t *testing.T) {
	t.Parallel()
	b, _ := trippedWithoutCanary(t)
	for _, in := range []bucketInput{
		{runID: "r2", confirmed: 1, probed: -1},                                             // a non-sweep path
		{runID: "r2", confirmed: 1, probed: 300},                                            // a sweep with no sample
		{runID: "r2", confirmed: 1, probed: 300, sample: sweepSample{probed: 2}},            // too small a sample
		{runID: "r2", confirmed: 1, probed: 300, sample: sweepSample{probed: 5, broken: 1}}, // 20% broken
	} {
		out := b.evaluate(t.Context(), testBucket(), in)
		if out.hold != holdNoCanary || out.released {
			t.Errorf("evaluate(%+v) = %+v, want held as no_canary", in, out)
		}
		if state := b.buckets[testBucket()].state; state != breakerTripped {
			t.Errorf("state = %s, want tripped", state)
		}
	}
}

func TestSweepSampleStandsInForTheCanary(t *testing.T) {
	t.Parallel()
	b, _ := trippedWithoutCanary(t)
	out := b.evaluate(t.Context(), testBucket(), bucketInput{
		runID: "r2", confirmed: 1, probed: 300, sample: sweepSample{probed: 40, broken: 3},
	})
	if !out.released || out.hold != holdNone {
		t.Fatalf("outcome = %+v, want released on a sample of 40 non-held entries with 7.5%% broken", out)
	}
	// The tripping run itself never counts as the later run.
	b2, _ := trippedWithoutCanary(t)
	out = b2.evaluate(t.Context(), testBucket(), bucketInput{
		runID: "r1", confirmed: 1, probed: 300, sample: sweepSample{probed: 40},
	})
	if out.released {
		t.Fatalf("the tripping run released its own bucket: %+v", out)
	}
}

func TestAClosedBucketWithoutACanaryStillAdmits(t *testing.T) {
	t.Parallel()
	verdict := canaryNone
	b, _, _ := testBreaker(&verdict)
	if out := b.evaluate(
		t.Context(),
		testBucket(),
		bucketInput{runID: "r", confirmed: 1, probed: 100},
	); out.hold != holdNone {
		t.Fatalf("outcome = %+v, want admitted", out)
	}
}

func TestOperatorForceReleasesABucketHeldAsNoCanary(t *testing.T) {
	t.Parallel()
	service, clock := newAdmitService(t)
	for i := range 10 {
		_ = service.Admit(
			t.Context(),
			HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: fmt.Sprintf("e%d", i)},
		)
	}
	clock.Advance(3 * time.Hour)
	var held *HeldError
	if err := service.Admit(
		t.Context(),
		HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "late"},
	); !errors.As(
		err, &held) ||
		held.Reason != string(holdNoCanary) {
		t.Fatalf("setup: admit = %v, want held as no_canary", err)
	}
	err := service.Admit(t.Context(), HealRequest{
		Provider: "rd", Class: ClassHosterLink, Subject: "late", Operator: true, Force: true, Caller: "ops",
	})
	if err != nil {
		t.Fatalf("forced admit = %v", err)
	}
	if len(service.BreakerStatus()) != 0 {
		t.Fatalf("a forced heal left the bucket open: %+v", service.BreakerStatus())
	}
}

// Non-sweep paths hitting a tripped bucket with nothing to probe keep holding, and the status says what they wait for.
func TestNonSweepPathsKeepHoldingWithoutACanary(t *testing.T) {
	t.Parallel()
	service, clock := newAdmitService(t)
	subjects := func(prefix string) {
		for i := range 10 {
			_ = service.Admit(t.Context(), HealRequest{
				Provider: "rd", Class: ClassHosterLink, Subject: fmt.Sprintf("%s%d", prefix, i),
			})
		}
	}
	subjects("e")
	clock.Advance(3 * time.Hour)
	err := service.Admit(t.Context(), HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "late"})
	var held *HeldError
	if !errors.As(err, &held) || held.Reason != string(holdNoCanary) {
		t.Fatalf("admit = %v, want held for no_canary", err)
	}
	status := service.BreakerStatus()
	if len(status) != 1 || status[0].Reason != "no_canary" || status[0].Hint != "waiting for a sweep or force" {
		t.Fatalf("status = %+v, want no_canary waiting for a sweep or force", status)
	}
}

// A releasing bucket with no canary has only the sweep's sample as evidence, so a drain sweep whose non-held sample
// is broken trips it again as no_canary, and only a later passing sample releases it (R0.1 run 4, finding 1): a
// later sweep with too small a sample must not drain the backlog the broken one held.
func TestDrainWithoutACanaryTripsOnABrokenSample(t *testing.T) {
	t.Parallel()
	b, clock := trippedWithoutCanary(t)
	out := b.evaluate(t.Context(), testBucket(), bucketInput{
		runID: "r2", confirmed: 80, held: 80, probed: 100, sample: sweepSample{probed: 20},
	})
	if !out.released {
		t.Fatalf("release sweep = %+v, want released on a clean sample", out)
	}
	clock.Advance(25 * time.Hour)
	out = b.evaluate(t.Context(), testBucket(), bucketInput{
		runID: "r3", confirmed: 64, held: 55, probed: 64, sample: sweepSample{probed: 9, broken: 9},
	})
	if out.draining || out.hold != holdNoCanary || !out.newlyTripped {
		t.Errorf("drain sweep with a broken sample = %+v, want tripped as no_canary", out)
	}
	if state := b.buckets[testBucket()].state; state != breakerTripped {
		t.Errorf("state = %s, want tripped", state)
	}
	clock.Advance(25 * time.Hour)
	out = b.evaluate(t.Context(), testBucket(), bucketInput{
		runID: "r4", confirmed: 66, held: 64, probed: 66, sample: sweepSample{probed: 2, broken: 2},
	})
	if out.draining || out.released || out.hold == holdNone {
		t.Errorf("sweep with too small a sample = %+v, want held", out)
	}
	clock.Advance(25 * time.Hour)
	out = b.evaluate(t.Context(), testBucket(), bucketInput{
		runID: "r5", confirmed: 64, held: 64, probed: 84, sample: sweepSample{probed: 20},
	})
	if !out.released || !out.draining {
		t.Errorf("sweep with a clean sample = %+v, want released and draining", out)
	}
}
