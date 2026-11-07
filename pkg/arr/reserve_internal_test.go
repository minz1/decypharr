package arr

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// Callers admitted at the same time are counted against each other, not against what has been sent so far.
func TestConcurrentAdmitsAreReservedAgainstEachOther(t *testing.T) {
	t.Parallel()
	service, _, hooks := guardedService(t, newResearchArr())
	hooks.setVerdict(CanaryFail)

	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := range 60 {
		wg.Go(func() {
			if err := service.AdmitFailGrab(t.Context(), "tv", fmt.Sprintf("hash-%03d", i), "realdebrid"); err == nil {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if got := admitted.Load(); got != 20 {
		t.Fatalf("%d concurrent grabs were admitted before any was sent, want the 20 free ones", got)
	}
}

// A grab that was admitted but not sent gives its place back.
func TestReleasedReservationFreesItsPlace(t *testing.T) {
	t.Parallel()
	service, _, hooks := guardedService(t, newResearchArr())
	hooks.setVerdict(CanaryFail)
	for i := range 20 {
		if err := service.AdmitFailGrab(t.Context(), "tv", fmt.Sprintf("hash-%03d", i), "realdebrid"); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.AdmitFailGrab(t.Context(), "tv", "hash-extra", "realdebrid"); err == nil {
		t.Fatal("a 21st reservation passed")
	}
	service.ReleaseFailGrab("tv", "hash-000", "realdebrid")
	if err := service.AdmitFailGrab(t.Context(), "tv", "hash-extra", "realdebrid"); err != nil {
		t.Fatalf("the place of a released reservation was not freed: %v", err)
	}
	// A grab that was committed is not given back by a later release.
	service.CommitFailGrab("tv", "hash-001", "realdebrid")
	service.ReleaseFailGrab("tv", "hash-001", "realdebrid")
	if err := service.AdmitFailGrab(t.Context(), "tv", "hash-another", "realdebrid"); err == nil {
		t.Fatal("releasing a committed grab freed its place")
	}
}
