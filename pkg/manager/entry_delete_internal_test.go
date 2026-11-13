package manager

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestDeleteDuringSubmitReleasesTheLatePlacement(t *testing.T) {
	t.Parallel()
	for _, second := range []bool{false, true} {
		t.Run(map[bool]string{false: "post lands after the delete", true: "post lands after a second delete"}[second],
			func(t *testing.T) {
				t.Parallel()
				store := newTestStore(t)
				synctest.Test(t, func(t *testing.T) { lateDelete(t, store, second) })
			})
	}
}

func lateDelete(t *testing.T, store *storage.Storage, second bool) {
	t.Helper()
	h := newActorHarness(t, store)
	m := h.manager()
	submit := h.submitting("a")
	want := grab.Cleanup{DeleteFiles: true, ReleaseDelete: true}

	if err := m.DeleteQueued(t.Context(), "A", want); err != nil {
		t.Fatal(err)
	}
	if second {
		if err := m.DeleteQueued(t.Context(), "a", grab.Cleanup{}); err != nil {
			t.Fatal(err)
		}
	}
	h.none()
	if got := h.phase("a"); got != grab.PhaseDeleting {
		t.Fatalf("phase %v while the post is out, want deleting", got)
	}

	reply(submit, grab.Event{
		Kind: grab.EvSubmitResult, Outcome: grab.OutcomePlacement, Provider: "realdebrid", TorrentID: "t1",
	})
	cleanup := h.next(grab.ChildCleanup)
	if refs := []placementRef{{Provider: "realdebrid", TorrentID: "t1"}}; !slices.Equal(cleanup.call.releases, refs) {
		t.Fatalf("cleanup releases %v, want %v", cleanup.call.releases, refs)
	}
	if cleanup.call.cleanup != want {
		t.Fatalf("cleanup %+v, want %+v", cleanup.call.cleanup, want)
	}
	reply(cleanup, grab.Event{Kind: grab.EvCleanupDone})
	h.none()
	if got := h.diskPhase(); got != grab.PhaseAbsent {
		t.Fatalf("disk phase %v, want absent", got)
	}
	if got := h.phase("a"); got != grab.PhaseAbsent {
		t.Fatalf("phase %v, want absent", got)
	}
}

func TestDeleteThenReAddIsTwoGrabsNotOne(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		m := h.manager()
		old := testEntry("a")
		old.Providers = map[string]*storage.ProviderEntry{"torbox": {Provider: "torbox", ID: "tb1"}}
		seed(t, store, old, grab.State{Phase: grab.PhaseComplete})

		if err := m.DeleteQueued(t.Context(), "a", grab.Cleanup{}); err != nil {
			t.Fatal(err)
		}
		cleanup := h.next(grab.ChildCleanup)
		if err := m.submitEntry(t.Context(), testEntry("a"), addReq()); err == nil {
			t.Fatal("a re-add while the delete cleans up was accepted")
		}
		h.none()

		reply(cleanup, grab.Event{Kind: grab.EvCleanupDone})
		synctest.Wait()
		if got := h.phase("a"); got != grab.PhaseAbsent {
			t.Fatalf("phase %v after the cleanup, want absent", got)
		}
		if err := m.submitEntry(t.Context(), testEntry("a"), addReq()); err != nil {
			t.Fatal(err)
		}
		h.next(grab.ChildSubmit)
		got, err := store.GetQueuedGrab("a")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Providers) != 0 {
			t.Fatalf("the re-add kept the deleted grab's placements: %+v", got.Providers)
		}
		if phase := h.phase("a"); phase != grab.PhaseSubmitting {
			t.Fatalf("phase %v after the re-add, want submitting", phase)
		}
	})
}

func TestTagsChangeKeepsThePhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []grab.Phase{grab.PhaseProvider, grab.PhaseComplete, grab.PhaseFailed} {
		t.Run(phase.String(), func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) { tagsKeepPhase(t, store, phase) })
		})
	}
}

func tagsKeepPhase(t *testing.T, store *storage.Storage, phase grab.Phase) {
	t.Helper()
	h := newActorHarness(t, store)
	m := h.manager()
	st := grab.State{Phase: phase}
	if phase == grab.PhaseFailed {
		st = failedState()
	}
	seed(t, store, testEntry("a"), st)

	if err := m.SetTags(t.Context(), "a", []string{"x", "y"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCategory(t.Context(), "a", "radarr"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetQueuedGrab("a")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Tags, []string{"x", "y"}) || got.Category != "radarr" {
		t.Fatalf("tags %v category %q on disk, want [x y] radarr", got.Tags, got.Category)
	}
	if err = m.SetTags(t.Context(), "a", nil); err != nil {
		t.Fatal(err)
	}
	if got, err = store.GetQueuedGrab("a"); err != nil || len(got.Tags) != 0 {
		t.Fatalf("tags %v after clearing, err %v, want none", got.Tags, err)
	}
	if h.phase("a") != phase || h.diskPhase() != phase {
		t.Fatalf("phase %v on disk %v, want %v kept", h.phase("a"), h.diskPhase(), phase)
	}
	h.none()
}

func TestProgressIsDebounced(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseActing)
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildAction)

		put := func(p float64) {
			h.send(entryMsg{
				ev: &grab.Event{Kind: grab.EvActionProgress}, progress: p,
				update: func(e *storage.Entry) { e.IsDownloading = true },
			})
		}
		onDisk := func() float64 {
			e, err := store.GetQueuedGrab("a")
			if err != nil {
				t.Fatal(err)
			}
			return e.Progress
		}
		put(0.10)
		if got := onDisk(); got != 0.10 {
			t.Fatalf("first progress on disk %v, want 0.10", got)
		}
		put(0.105)
		if got := onDisk(); got != 0.10 {
			t.Fatalf("a 0.5%% step inside 5s reached disk: %v", got)
		}
		put(0.12)
		if got := onDisk(); got != 0.12 {
			t.Fatalf("a 2%% step on disk %v, want 0.12", got)
		}
		put(0.121)
		time.Sleep(5 * time.Second)
		put(0.122)
		if got := onDisk(); got != 0.122 {
			t.Fatalf("progress after 5s on disk %v, want 0.122", got)
		}
		if h.phase("a") != grab.PhaseActing {
			t.Fatalf("phase %v, want acting", h.phase("a"))
		}
	})
}

func TestSweepRemovesFailedThroughTheActor(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		m := h.manager()
		m.queue = &Queue{storage: store, logger: zerolog.Nop(), removeStalledAfter: time.Hour}
		seed(t, store, testEntry("a"), failedState())
		seedPhase(t, store, "b", grab.PhaseQueued)
		time.Sleep(2 * time.Hour)

		if err := m.sweepStalled(func(string) bool { return false }); err != nil {
			t.Fatal(err)
		}
		cleanup := h.next(grab.ChildCleanup)
		if cleanup.call.cleanup != (grab.Cleanup{}) {
			t.Fatalf("cleanup %+v, want the zero cleanup", cleanup.call.cleanup)
		}
		other := h.next(grab.ChildCleanup)
		if other.call.cleanup != (grab.Cleanup{}) {
			t.Fatalf("cleanup %+v, want the zero cleanup", other.call.cleanup)
		}
		if got := h.diskPhase(); got != grab.PhaseDeleting {
			t.Fatalf("disk phase %v before the cleanup finished, want deleting", got)
		}
		reply(cleanup, grab.Event{Kind: grab.EvCleanupDone})
		reply(other, grab.Event{Kind: grab.EvCleanupDone})
		synctest.Wait()
		if got := h.diskPhase(); got != grab.PhaseAbsent {
			t.Fatalf("disk phase %v, want absent", got)
		}
		if _, err := store.GetQueuedGrab("b"); err == nil {
			t.Fatal("the stalled queued grab was not removed")
		}
	})
}

func TestDeleteCancelsTheRunningAction(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseActing)
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildAction)

		if err := h.manager().DeleteQueued(t.Context(), "A", grab.Cleanup{}); err != nil {
			t.Fatal(err)
		}
		h.next(grab.ChildCleanup)
	})
}
