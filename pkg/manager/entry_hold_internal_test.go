package manager

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func requireHolds(t *testing.T, h *actorHarness, want ...string) {
	t.Helper()
	synctest.Wait()
	if got := h.held(); !slices.Equal(got, want) {
		t.Fatalf("holder calls = %v, want %v", got, want)
	}
}

func adopt(run childRun) {
	run.reply <- entryMsg{
		ev: &grab.Event{
			Kind: grab.EvSubmitResult, Outcome: grab.OutcomePlacement, Provider: "realdebrid", TorrentID: "t1",
		},
		torrent: testTorrent(),
	}
}

func TestAdoptedPlacementIsHeld(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		submit := h.submitting("a")
		requireHolds(t, h)

		adopt(submit)
		requireHolds(t, h, "hold:realdebrid/t1/a")
	})
}

func TestARefusedHoldIsRetriedByTheNextStep(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		submit := h.submitting("a")
		h.mu.Lock()
		h.holdErr = errors.New("refused")
		h.mu.Unlock()

		adopt(submit)
		requireHolds(t, h)
		h.mu.Lock()
		h.holdErr = nil
		h.mu.Unlock()
		if err := h.ask("a", entryMsg{ev: &grab.Event{Kind: grab.EvTagsChanged}, tags: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
		requireHolds(t, h, "hold:realdebrid/t1/a")
	})
}

func TestRemovingTheRecordReleasesTheHold(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		submit := h.submitting("a")
		adopt(submit)
		synctest.Wait()
		h.mustAsk("a", grab.Event{Kind: grab.EvDelete})
		h.next(grab.ChildPoll)
		cleanup := h.next(grab.ChildCleanup)
		requireHolds(t, h, "hold:realdebrid/t1/a")

		reply(cleanup, grab.Event{Kind: grab.EvCleanupDone})
		requireHolds(t, h, "hold:realdebrid/t1/a", "unhold:realdebrid/t1/a")
	})
}

func TestReachingCompleteReleasesTheHold(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		e := testEntry("a")
		e.Providers = map[string]*storage.ProviderEntry{
			"realdebrid": {Provider: "realdebrid", ID: "t1", Status: debridTypes.TorrentStatusDownloaded},
		}
		seed(t, store, e, grab.State{Phase: grab.PhaseActing})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		requireHolds(t, h, "hold:realdebrid/t1/a")

		reply(h.next(grab.ChildAction), grab.Event{Kind: grab.EvActionDone})
		requireHolds(t, h, "hold:realdebrid/t1/a", "unhold:realdebrid/t1/a")
	})
}

func TestAPlacementGoneForAnotherTorrentIsDropped(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		submit := h.submitting("a")
		adopt(submit)
		synctest.Wait()

		h.mustAsk("a", grab.Event{
			Kind: grab.EvPlacementGone, Fail: grab.FailNoEffect, Provider: "realdebrid", TorrentID: "old",
		})
		if got := h.phase("a"); got != grab.PhaseProvider {
			t.Fatalf("phase %v after a stale placement-gone, want provider", got)
		}
	})
}

func TestAChangedPlacementReleasesTheOldTorrentThenHoldsTheNew(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		placed := func(id string) *storage.Entry {
			e := testEntry("a")
			e.Providers = map[string]*storage.ProviderEntry{
				"realdebrid": {Provider: "realdebrid", ID: id, Status: debridTypes.TorrentStatusDownloaded},
			}
			return e
		}
		seed(t, store, placed("t1"), grab.State{Phase: grab.PhaseActing})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		requireHolds(t, h, "hold:realdebrid/t1/a")

		switched := entryMsg{ev: &grab.Event{Kind: grab.EvTagsChanged}, update: func(e *storage.Entry) {
			e.Providers["realdebrid"].ID = "t2"
		}}
		if err := h.ask("a", switched); err != nil {
			t.Fatal(err)
		}

		requireHolds(t, h, "hold:realdebrid/t1/a", "unhold:realdebrid/t1/a", "hold:realdebrid/t2/a")
	})
}
