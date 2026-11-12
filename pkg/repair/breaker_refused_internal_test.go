package repair

import (
	"testing"
	"time"
)

// A bucket's illegal move is counted on the breaker and reported by the service.
func TestBreakerCountsIllegalBucketMoves(t *testing.T) {
	t.Parallel()
	b, _, _ := testBreaker(new(canaryOutcome))
	if err := b.bucketLocked(testBucket()).transition(breakerReleasing, time.Now(), "", holdNone); err == nil {
		t.Fatal("Closed → Releasing was allowed")
	}
	if got := (&Service{breaker: b}).Refused(); got != 1 {
		t.Fatalf("Refused() = %d, want 1", got)
	}
}
