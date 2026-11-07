package arr

import (
	"fmt"
	"testing"
)

// A probe the repair service cut off holds the excess for the next window without tripping the bucket, and the next
// caller asks again instead of reading a cached non-answer.
func TestAnAbortedGuardCanaryHoldsWithoutTrippingOrCaching(t *testing.T) {
	t.Parallel()
	service, _, hooks := guardedService(t, newResearchArr())
	hooks.setVerdict(CanaryAborted)
	for i := range 20 {
		if err := service.AdmitFailGrab(t.Context(), "tv", fmt.Sprintf("free-%02d", i), "realdebrid"); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.AdmitFailGrab(t.Context(), "tv", "excess", "realdebrid"); err == nil {
		t.Fatal("a download beyond the free allowance passed on an aborted canary")
	}
	if status := service.QueueGuardStatus(); len(status) != 0 || hooks.noticeCount() != 0 {
		t.Fatalf("status %+v, notices %d, want no trip", status, hooks.noticeCount())
	}

	hooks.setVerdict(CanaryPass)
	if err := service.AdmitFailGrab(t.Context(), "tv", "excess", "realdebrid"); err != nil {
		t.Fatalf("the next caller was held by a cached aborted verdict: %v", err)
	}
	if hooks.canaryCalls() != 2 {
		t.Fatalf("canary calls = %d, want the aborted one and a fresh one", hooks.canaryCalls())
	}
}
