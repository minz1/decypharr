package arr

import (
	"testing"
	"time"
)

// A bucket's illegal move is counted on the guard and reported by the service.
func TestQueueGuardCountsIllegalBucketMoves(t *testing.T) {
	t.Parallel()
	g := newQueueGuard()
	if err := g.bucketLocked(guardKey{arr: "sonarr"}).transition(guardClosed, time.Now()); err == nil {
		t.Fatal("Closed → Closed was allowed")
	}
	if got := (&Service{guard: g}).Refused(); got != 1 {
		t.Fatalf("Refused() = %d, want 1", got)
	}
}
