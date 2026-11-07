package arr

import (
	"fmt"
	"testing"
	"time"
)

// A canary with nothing to probe is no evidence the provider works: the excess waits for the next window, without
// tripping the bucket or announcing anything.
func TestNoCanarySignalHoldsTheExcessWithoutATrip(t *testing.T) {
	t.Parallel()
	var records []QueueSchema
	for i := range 50 {
		records = append(records, failedRows(fmt.Sprintf("dl-%02d", i), 1+i*2, 2)...)
	}
	fake := newResearchArr(records...)
	service, clock, hooks := guardedService(t, fake)
	hooks.setVerdict(CanaryNone)

	runCleanup(t, service, "tv", 3)
	if got := len(blocklisted(fake)); got != 40 {
		t.Fatalf("blocklisted %d rows, want the 20 free downloads only", got)
	}
	if hooks.noticeCount() != 0 {
		t.Fatalf("notifications = %d, want none: nothing failed, there was just nothing to probe", hooks.noticeCount())
	}
	if status := service.QueueGuardStatus(); len(status) != 0 {
		t.Fatalf("guard status = %+v, want the bucket still closed", status)
	}

	// The next window's free allowance serves the oldest held downloads.
	clock.advance(61 * time.Minute)
	runCleanup(t, service, "tv", 1)
	if got := len(blocklisted(fake)); got != 80 {
		t.Fatalf("blocklisted %d rows after the next window, want 40 more", got)
	}
}
