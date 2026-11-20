package arr

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func guardWindows(service *Service) (time.Duration, time.Duration) {
	window := service.config.Get().QueueCleanup.BlocklistLimits().Window
	return window, service.config.Get().Repair.BreakerSettings().CanaryCache
}

func heldRetryAt(t *testing.T, err error) time.Time {
	t.Helper()
	held, ok := errors.AsType[*HeldError](err)
	if !ok {
		t.Fatalf("error = %v, want a *HeldError", err)
	}
	if !errors.Is(err, ErrBlocklistHeld) {
		t.Fatalf("error %v does not wrap ErrBlocklistHeld", err)
	}
	return held.RetryAt
}

func TestDecideRetryAtNonCanary(t *testing.T) {
	t.Parallel()
	service, clock, _ := guardedService(t, newResearchArr())
	window, ttl := guardWindows(service)
	limits := service.config.Get().QueueCleanup.BlocklistLimits()
	items := make([]QueueSchema, limits.PerBucket+1)
	for i := range items {
		items[i] = QueueSchema{
			ID:         i + 1,
			DownloadID: fmt.Sprintf("hash-%03d", i),
			rule:       "import_end:wrong_episode",
			provider:   "usenet",
		}
	}
	now := clock.Now()
	decision, need := service.guard.decide("tv", groupUnits("tv", items), limits, ttl, now, nil)
	if len(need) != 0 {
		t.Fatalf("need = %v, want none for a rule without a canary", need)
	}
	last := items[len(items)-1].ID
	if decision.allowed[last] {
		t.Fatal("the download beyond the free allowance was admitted")
	}
	if want := now.Add(window + time.Nanosecond); !decision.retryAt[last].Equal(want) {
		t.Fatalf("retryAt = %v, want %v", decision.retryAt[last], want)
	}
	if _, ok := decision.retryAt[1]; ok {
		t.Fatal("an admitted row has a retry time")
	}
}

func fillBucket(t *testing.T, service *Service, hooks *guardHooks, verdict CanaryVerdict) {
	t.Helper()
	hooks.setVerdict(CanaryPass)
	if sent := failGrabs(t, service, 0, 20); sent != 20 {
		t.Fatalf("sent %d, want the 20 free ones", sent)
	}
	hooks.setVerdict(verdict)
}

func TestHeldErrorRetryAtCanaryFail(t *testing.T) {
	t.Parallel()
	service, clock, hooks := guardedService(t, newResearchArr())
	window, ttl := guardWindows(service)
	start := clock.Now()
	fillBucket(t, service, hooks, CanaryFail)
	clock.advance(time.Minute)
	probedAt := clock.Now()
	err := service.AdmitFailGrab(t.Context(), "tv", "hash-late", "realdebrid")
	want := minTime(start.Add(window+time.Nanosecond), probedAt.Add(ttl))
	if got := heldRetryAt(t, err); !got.Equal(want) {
		t.Fatalf("retryAt = %v, want %v", got, want)
	}
	clock.advance(time.Minute)
	cached := service.AdmitFailGrab(t.Context(), "tv", "hash-late", "realdebrid")
	if got := heldRetryAt(t, cached); !got.Equal(want) {
		t.Fatalf("retryAt on a cached verdict = %v, want %v", got, want)
	}
	if hooks.canaryCalls() != 1 {
		t.Fatalf("canary probes = %d, want one", hooks.canaryCalls())
	}
}

func TestHeldErrorRetryAtInconclusive(t *testing.T) {
	t.Parallel()
	service, clock, hooks := guardedService(t, newResearchArr())
	_, ttl := guardWindows(service)
	fillBucket(t, service, hooks, CanaryInconclusive)
	probedAt := clock.Now()
	err := service.AdmitFailGrab(t.Context(), "tv", "hash-late", "realdebrid")
	if got, want := heldRetryAt(t, err), probedAt.Add(ttl); !got.Equal(want) {
		t.Fatalf("retryAt = %v, want the verdict expiry %v", got, want)
	}
}

func TestHeldErrorRetryAtFreshAbort(t *testing.T) {
	t.Parallel()
	service, clock, hooks := guardedService(t, newResearchArr())
	window, ttl := guardWindows(service)
	start := clock.Now()
	fillBucket(t, service, hooks, CanaryFail)
	if err := service.AdmitFailGrab(t.Context(), "tv", "hash-late", "realdebrid"); err == nil {
		t.Fatal("grab beyond the allowance admitted")
	}
	clock.advance(ttl + time.Minute)
	hooks.setVerdict(CanaryAborted)
	err := service.AdmitFailGrab(t.Context(), "tv", "hash-late", "realdebrid")
	if got, want := heldRetryAt(t, err), start.Add(window+time.Nanosecond); !got.Equal(want) {
		t.Fatalf("retryAt = %v, want the bucket drain %v, not the stale verdict", got, want)
	}
}

func TestHeldErrorRetryAtAbortWithEmptyBucket(t *testing.T) {
	t.Parallel()
	service, clock, _ := guardedService(t, newResearchArr())
	_, ttl := guardWindows(service)
	now := clock.Now()
	limits := service.config.Get().QueueCleanup.BlocklistLimits()
	limits.PerBucket = 0
	units := groupUnits("tv", []QueueSchema{{ID: 1, DownloadID: "hash", rule: failGrabRule, provider: "realdebrid"}})
	decision, need := service.guard.decide(
		"tv",
		units,
		limits,
		ttl,
		now,
		map[string]CanaryVerdict{"realdebrid": CanaryAborted},
	)
	if len(need) != 0 {
		t.Fatalf("need = %v", need)
	}
	if want := now.Add(ttl); !decision.retryAt[1].Equal(want) {
		t.Fatalf("retryAt = %v, want %v", decision.retryAt[1], want)
	}
}

func fillArrCap(t *testing.T, service *Service) {
	t.Helper()
	for i := range 100 {
		id := fmt.Sprintf("hash-%03d", i)
		provider := fmt.Sprintf("provider-%d", i%13)
		if err := service.AdmitFailGrab(t.Context(), "tv", id, provider); err != nil {
			t.Fatalf("AdmitFailGrab(%s) = %v", id, err)
		}
		service.CommitFailGrab("tv", id, provider)
	}
}

func TestHeldErrorRetryAtArrCap(t *testing.T) {
	t.Parallel()
	service, clock, _ := guardedService(t, newResearchArr())
	window, _ := guardWindows(service)
	start := clock.Now()
	fillArrCap(t, service)
	err := service.AdmitFailGrab(t.Context(), "tv", "hash-extra", "provider-x")
	if got, want := heldRetryAt(t, err), start.Add(window+time.Nanosecond); !got.Equal(want) {
		t.Fatalf("retryAt = %v, want %v", got, want)
	}
}

func TestAdmitAtRetryAt(t *testing.T) {
	t.Parallel()
	service, clock, _ := guardedService(t, newResearchArr())
	fillArrCap(t, service)
	retryAt := heldRetryAt(t, service.AdmitFailGrab(t.Context(), "tv", "hash-extra", "provider-x"))
	clock.advance(retryAt.Add(-time.Nanosecond).Sub(clock.Now()))
	if err := service.AdmitFailGrab(t.Context(), "tv", "hash-extra", "provider-x"); err == nil {
		t.Fatal("admitted before RetryAt")
	}
	clock.advance(time.Nanosecond)
	if err := service.AdmitFailGrab(t.Context(), "tv", "hash-extra", "provider-x"); err != nil {
		t.Fatalf("at RetryAt: %v", err)
	}
}
