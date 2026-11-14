package manager

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

func (h *actorHarness) drainCleanups() []string {
	h.t.Helper()
	var cleaned []string
	for len(h.started) > 0 {
		run := <-h.started
		if run.call.kind != grab.ChildCleanup {
			h.t.Fatalf("started %v, want a cleanup", childKind(run.call.kind))
		}
		cleaned = append(cleaned, run.call.grab.Entry.InfoHash)
		reply(run, grab.Event{Kind: grab.EvCleanupDone})
	}
	slices.Sort(cleaned)
	return cleaned
}

func TestDeleteFailedNZBsDeletesOnlyFailed(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		nzb := func(key, category string, phase grab.Phase) {
			e := testEntry(key)
			e.Category, e.Protocol = category, config.ProtocolNZB
			seed(t, store, e, phaseState(phase, true))
		}
		nzb("failed", "sonarr", grab.PhaseFailed)
		nzb("complete", "sonarr", grab.PhaseComplete)
		nzb("provider", "sonarr", grab.PhaseProvider)
		nzb("other", "radarr", grab.PhaseFailed)
		torrent := testEntry("torrent")
		seed(t, store, torrent, phaseState(grab.PhaseFailed, false))
		orphan := testEntry("orphan")
		orphan.Category, orphan.Protocol = "sonarr", config.ProtocolNZB
		if err := store.UpdateQueue(orphan); err != nil {
			t.Fatal(err)
		}
		recoverAll(h)
		h.next(grab.ChildPoll)

		m := h.manager()
		m.queue = newQueue(store, nil, h.grabs, zerolog.Nop())
		if err := m.DeleteFailedNZBs(t.Context(), "sonarr"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		cleaned := h.drainCleanups()
		if want := []string{"failed", "orphan"}; !slices.Equal(cleaned, want) {
			t.Fatalf("cleaned %v, want %v", cleaned, want)
		}
		synctest.Wait()
		for _, key := range []string{"complete", "provider", "other", "torrent"} {
			if _, ok := h.diskGrab(key); !ok {
				t.Errorf("%s was deleted", key)
			}
		}
		for _, key := range cleaned {
			if _, ok := h.diskGrab(key); ok {
				t.Errorf("%s is still on disk", key)
			}
		}
	})
}
