package arr

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// failGrabs admits and sends the grab failure of count downloads, returning how many passed.
func failGrabs(t *testing.T, service *Service, first, count int) int {
	t.Helper()
	sent := 0
	for i := first; i < first+count; i++ {
		id := fmt.Sprintf("hash-%03d", i)
		err := service.AdmitFailGrab(t.Context(), "tv", id, "realdebrid")
		switch {
		case err == nil:
			service.CommitFailGrab("tv", id, "realdebrid")
			sent++
		case !errors.Is(err, ErrBlocklistHeld):
			t.Fatalf("AdmitFailGrab(%s) = %v", id, err)
		}
	}
	return sent
}

// A storm of failed grabs past 20 an hour is let through only while the provider's canary passes.
func TestFailGrabStormWithAFailingCanaryBlocklistsNothingPastTwenty(t *testing.T) {
	t.Parallel()
	for name, verdict := range map[string]CanaryVerdict{"failing": CanaryFail, "inconclusive": CanaryInconclusive} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			service, clock, hooks := guardedService(t, newResearchArr())
			hooks.setVerdict(verdict)

			if sent := failGrabs(t, service, 0, 50); sent != 20 {
				t.Fatalf("failed %d grabs, want the 20 free ones", sent)
			}
			if hooks.noticeCount() != 1 {
				t.Fatalf("notifications = %d, want one for the trip", hooks.noticeCount())
			}
			if status := service.QueueGuardStatus(); len(status) != 1 || status[0].Rule != "fail_grab" ||
				status[0].State != "tripped" || status[0].Provider != "realdebrid" {
				t.Fatalf("guard status = %+v, want fail_grab tripped for realdebrid", status)
			}

			// A held grab is retried later; once the provider passes it goes through, and only once.
			hooks.setVerdict(CanaryPass)
			clock.advance(16 * time.Minute)
			if sent := failGrabs(t, service, 20, 30); sent != 30 {
				t.Fatalf("retried %d held grabs, want all 30 after the canary passes", sent)
			}
			if sent := failGrabs(t, service, 0, 50); sent != 50 {
				t.Fatalf("a repeat of already-sent grabs passed %d, want 50: they cost nothing again", sent)
			}
		})
	}
}

// Grabs and queue cleanup share the arr's hard cap of 100 distinct downloads an hour.
func TestFailGrabsShareTheArrHardCapWithQueueCleanup(t *testing.T) {
	t.Parallel()
	service, clock, _ := guardedService(t, newResearchArr())
	_ = clock
	passed := 0
	for i := range 130 {
		id := fmt.Sprintf("hash-%03d", i)
		provider := fmt.Sprintf("provider-%d", i%13) // 10 per provider: under 20 each
		if err := service.AdmitFailGrab(t.Context(), "tv", id, provider); err == nil {
			service.CommitFailGrab("tv", id, provider)
			passed++
		}
	}
	if passed != 100 {
		t.Fatalf("%d grabs failed in an hour, want the arr cap of 100", passed)
	}
}
