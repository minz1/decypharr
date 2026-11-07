package repair

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// recheckMedia runs a media recheck for the first library entry and waits for it.
func (f *outageFixture) recheckMedia(t *testing.T, opts RecheckOptions) {
	t.Helper()
	f.recheckMediaAfter(t, 2*time.Hour, opts)
}

// recheckMediaAfter runs the recheck the given time after the previous activity.
func (f *outageFixture) recheckMediaAfter(t *testing.T, after time.Duration, opts RecheckOptions) {
	t.Helper()
	f.clock.Advance(after)
	if _, err := f.service.RecheckMedia(t.Context(), "radarr", "1", opts); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
}

// trip makes the provider's hoster bucket trip: 80 of 100 releases fail on two sweeps, then recover enough that
// only entry 0 stays down (it remains held).
func (f *outageFixture) trip(t *testing.T) {
	t.Helper()
	f.downRange(0, 80)
	f.sweep(t.Context(), nil)
	f.sweep(t.Context(), nil)
	if status := f.service.BreakerStatus(); len(status) != 1 || status[0].State != "tripped" {
		t.Fatalf("setup: breaker status = %+v", status)
	}
	f.downRange(0, 1)
}

func TestWebhookRecheckProbesOnlyOnTheFirstObservation(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 3, config.RepairConfig{})
	f.downRange(0, 1)
	f.recheckMedia(t, RecheckOptions{Fix: true, Source: SourceWebhook})
	if f.requests() != 0 {
		t.Fatalf("a webhook probe healed from one observation: %#v", f.reacquirer.requests)
	}
	h := f.health(t, 0)
	if h.Status != storage.HealthBroken || len(h.BrokenFiles) != 1 || h.BrokenFiles[0].FirstSeenAt.IsZero() {
		t.Fatalf("health = %#v, want a first observation (suspect) recorded", h)
	}
}

func TestWebhookHealsAnEntryTheGateAlreadyConfirmed(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 3, config.RepairConfig{})
	f.downRange(0, 1)
	f.sweep(t.Context(), nil) // first observation by a sweep, a day ago
	f.recheckMedia(t, RecheckOptions{Fix: true, Source: SourceWebhook})
	if f.requests() != 1 || f.reacquirer.requests[0].FileID != "id-release-000" {
		t.Fatalf("requests = %#v, want the already-observed entry healed", f.reacquirer.requests)
	}
}

func TestWebhookWithinTheGapDoesNotConfirm(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 3, config.RepairConfig{ConfirmGap: "6h"})
	f.downRange(0, 1)
	f.recheckMedia(t, RecheckOptions{Fix: true, Source: SourceWebhook})
	f.recheckMedia(t, RecheckOptions{Fix: true, Source: SourceWebhook}) // 2h after the first
	if f.requests() != 0 {
		t.Fatalf("two webhook probes %s apart healed: %#v", 2*time.Hour, f.reacquirer.requests)
	}
}

func TestWebhookNeverHonoursForce(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.trip(t)
	f.recheckMediaAfter(
		t,
		20*time.Minute,
		RecheckOptions{Fix: true, Source: SourceWebhook, Force: true, Caller: "tautulli"},
	)
	if f.requests() != 0 {
		t.Fatalf("a forced webhook healed past a tripped bucket: %#v", f.reacquirer.requests)
	}
}

func TestOperatorRecheckSkipsTheGateButPassesTheBreaker(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 3, config.RepairConfig{})
	f.downRange(0, 1)
	f.recheckMedia(t, RecheckOptions{Fix: true, Source: SourceOperator})
	if f.requests() != 1 {
		t.Fatalf("an operator recheck did not heal on its own probe: %#v", f.reacquirer.requests)
	}

	g := newOutageFixture(t, 100, config.RepairConfig{})
	g.trip(t)
	g.recheckMediaAfter(t, 20*time.Minute, RecheckOptions{Fix: true, Source: SourceOperator})
	if g.requests() != 0 {
		t.Fatalf("an operator recheck healed past a tripped bucket without force: %#v", g.reacquirer.requests)
	}
	if got := g.health(t, 0).Status; got != storage.HealthHeld {
		t.Fatalf("status = %s, want held", got)
	}
}

func TestOperatorForceHealsPastATrippedBucket(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.trip(t)
	f.recheckMediaAfter(
		t,
		20*time.Minute,
		RecheckOptions{Fix: true, Source: SourceOperator, Force: true, Caller: "ops"},
	)
	if f.requests() != 1 || f.reacquirer.requests[0].FileID != "id-release-000" {
		t.Fatalf("requests = %#v, want the forced heal", f.reacquirer.requests)
	}
	if got := f.health(t, 0).Status; got != storage.HealthBroken {
		t.Fatalf("a forced heal left the entry %s", got)
	}
	if status := f.service.BreakerStatus(); len(status) != 0 {
		t.Fatalf("an operator force releases the bucket: %+v", status)
	}
}

func TestOperatorRecheckEntryPassesTheBreaker(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.trip(t)
	if _, err := f.service.RecheckEntry(t.Context(), f.entries[0].Name, RecheckOptions{Fix: true}); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	if f.requests() != 0 {
		t.Fatalf("recheck-entry fix healed past a tripped bucket: %#v", f.reacquirer.requests)
	}
	if _, err := f.service.RecheckEntry(
		t.Context(),
		f.entries[0].Name,
		RecheckOptions{Fix: true, Force: true},
	); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	if f.requests() != 1 {
		t.Fatalf("recheck-entry with force healed %d entries, want 1", f.requests())
	}
}

func TestFixBrokenAllIsHeldWhenItWouldTripTheBreaker(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(0, 30)
	f.sweep(t.Context(), nil) // 30 broken rows, first observation

	run, err := f.service.FixBroken(t.Context(), nil, FixOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	if f.requests() != 0 {
		t.Fatalf("fix-all healed %d of 30 entries past the breaker: %v", f.requests(), run.Stats)
	}
	if got := f.countStatus(t, storage.HealthHeld); got != 30 {
		t.Fatalf("held = %d, want 30", got)
	}

	// Held entries are fixed only on request, with force.
	if _, err = f.service.FixBroken(t.Context(), nil, FixOptions{}); err == nil {
		t.Fatal("a fix without force took held entries")
	}
	if _, err = f.service.FixBroken(t.Context(), nil, FixOptions{Force: true, Caller: "ops"}); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	if f.requests() != 30 {
		t.Fatalf("forced fix-all healed %d, want 30", f.requests())
	}
}

func TestFixBrokenWithFewEntriesPassesTheBreaker(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.downRange(0, 3)
	f.sweep(t.Context(), nil)
	if _, err := f.service.FixBroken(t.Context(), []string{f.entries[1].Name}, FixOptions{}); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	if f.requests() != 1 || f.reacquirer.requests[0].FileID != "id-release-001" {
		t.Fatalf("requests = %#v, want the named entry", f.reacquirer.requests)
	}
}

func TestFixBrokenNamedEntryIsHeldByATrippedBucket(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 100, config.RepairConfig{})
	f.trip(t)
	// Entry 0 is held; a fresh broken row for another release in the same bucket is what the operator names.
	if err := f.store.SaveEntryHealth(&storage.EntryHealth{
		EntryName: f.entries[90].Name, Status: storage.HealthBroken,
		BrokenFiles: []storage.BrokenFile{{
			EntryName: f.entries[90].Name, FileName: "video.mkv", InfoHash: outageHash(90),
			ArrName: "radarr", Reason: "hoster_unavailable", Provider: "rd",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.FixBroken(t.Context(), []string{f.entries[90].Name}, FixOptions{}); err != nil {
		t.Fatal(err)
	}
	f.service.runWG.Wait()
	if f.requests() != 0 {
		t.Fatalf("a named fix healed past a tripped bucket: %#v", f.reacquirer.requests)
	}
}
