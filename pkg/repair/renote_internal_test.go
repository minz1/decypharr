package repair

import (
	"fmt"
	"testing"
	"time"
)

// A subject whose earlier sighting aged out of the window and that fails again is a new failure, not a retry.
func TestASubjectSeenAgainAfterTheWindowCountsAgain(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, _ := testBreaker(&verdict)
	note := func() {
		for i := range 10 {
			b.noteConfirmed(testBucket(), fmt.Sprintf("entry-%d", i), sighting{})
		}
	}
	note()
	clock.Advance(61 * time.Minute)
	note()
	if got := b.windowCount(testBucket()); got != 10 {
		t.Fatalf("window count = %d, want the 10 subjects counted again", got)
	}
}

// A held subject is a retry however long it has been waiting.
func TestAHeldSubjectIsNeverCountedAgain(t *testing.T) {
	t.Parallel()
	verdict := canaryPass
	b, clock, _ := testBreaker(&verdict)
	b.noteConfirmed(testBucket(), "entry", sighting{})
	b.markHeld(testBucket(), "entry", true)
	clock.Advance(61 * time.Minute)
	b.noteConfirmed(testBucket(), "entry", sighting{})
	if got := b.windowCount(testBucket()); got != 0 {
		t.Fatalf("window count = %d, want 0: a held subject's retry is not a new failure", got)
	}
}
