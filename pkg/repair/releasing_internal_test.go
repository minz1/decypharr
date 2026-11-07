package repair

import (
	"testing"
	"time"
)

// A bucket being released is judged again on every evaluation: a new surge sends it back to tripped.
func TestReleasingBucketTripsAgainWhenTheThresholdIsExceeded(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, _ := testBreaker(&verdict)
	bucket := testBucket()
	b.evaluate(t.Context(), bucket, bucketInput{runID: "r1", confirmed: 81, probed: 4065})
	clock.Advance(time.Hour)
	if out := b.evaluate(t.Context(), bucket, bucketInput{runID: "r2", confirmed: 2, probed: 4000}); !out.released {
		t.Fatalf("setup: the bucket was not released: %+v", out)
	}

	out := b.evaluate(t.Context(), bucket, bucketInput{runID: "r2", confirmed: 60, probed: 100})
	if out.hold != holdTripped || !out.newlyTripped {
		t.Fatalf("outcome = %+v, want the releasing bucket tripped again", out)
	}
	if state := b.buckets[bucket].state; state != breakerTripped {
		t.Fatalf("state = %s, want tripped", state)
	}
}
