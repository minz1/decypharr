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

func TestOperatorForceReleasesATrippedBucketAndIsLogged(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	for i := range 10 {
		_ = service.Admit(
			t.Context(),
			HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: fmt.Sprintf("e%d", i)},
		)
	}
	if len(service.BreakerStatus()) != 1 {
		t.Fatal("setup: the bucket did not trip")
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
