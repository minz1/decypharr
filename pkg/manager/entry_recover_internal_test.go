package manager

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func recoverAll(h *actorHarness) {
	h.t.Helper()
	m := h.manager()
	records, packOf := m.readQueue()
	m.recoverEntries(records, packOf)
}

func phaseState(phase grab.Phase, nzb bool) grab.State {
	st := grab.State{Phase: phase, NZB: nzb}
	switch phase {
	case grab.PhaseFailed:
		st.Failure = grab.Failure{Reason: "refused", At: time.Now()}
	case grab.PhaseDeleting:
		st.Cleanup = grab.Cleanup{DeleteFiles: true}
	case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseProvider,
		grab.PhaseAwaitingLinks, grab.PhaseActing, grab.PhaseComplete:
	}
	return st
}

func TestRecoverFromEveryPhase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		phase    grab.Phase
		torrent  grab.ChildKind
		nzb      grab.ChildKind
		wantNone bool
	}{
		{phase: grab.PhaseQueued, torrent: grab.ChildSubmit, nzb: grab.ChildSubmit},
		{phase: grab.PhaseThrottled, torrent: grab.ChildSubmit, nzb: grab.ChildSubmit},
		{phase: grab.PhaseSubmitting, torrent: grab.ChildLookup, nzb: grab.ChildSubmit},
		{phase: grab.PhaseProvider, torrent: grab.ChildPoll, nzb: grab.ChildPoll},
		{phase: grab.PhaseAwaitingLinks, torrent: grab.ChildLinks, nzb: grab.ChildLinks},
		{phase: grab.PhaseActing, torrent: grab.ChildAction, nzb: grab.ChildAction},
		{phase: grab.PhaseDeleting, torrent: grab.ChildCleanup, nzb: grab.ChildCleanup},
		{phase: grab.PhaseFailed, wantNone: true},
		{phase: grab.PhaseComplete, wantNone: true},
	}
	for _, tc := range cases {
		for _, nzb := range []bool{false, true} {
			name, want := fmt.Sprintf("%s/torrent", tc.phase), tc.torrent
			if nzb {
				name, want = fmt.Sprintf("%s/nzb", tc.phase), tc.nzb
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				recoverPhaseCase(t, tc.phase, nzb, want, tc.wantNone)
			})
		}
	}
}

func recoverPhaseCase(t *testing.T, phase grab.Phase, nzb bool, want grab.ChildKind, wantNone bool) {
	t.Helper()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		e := testEntry("a")
		if nzb {
			e.Protocol = config.ProtocolNZB
		}
		seed(t, store, e, phaseState(phase, nzb))
		recoverAll(h)
		if wantNone {
			h.none()
			return
		}
		h.next(want)
		if want == grab.ChildLookup && submitCount(h) != 0 {
			t.Fatal("a torrent that was submitting was posted again instead of looked up")
		}
	})
}

func TestRecoverOfALegacyRecord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		entry func() *storage.Entry
		child grab.ChildKind
	}{
		{"queued", func() *storage.Entry {
			e := testEntry("a")
			e.State, e.Status = storage.EntryStateDownloading, debridTypes.TorrentStatusQueued
			return e
		}, grab.ChildSubmit},
		{"on a provider", func() *storage.Entry {
			e := testEntry("a")
			e.State, e.Status, e.ActiveProvider = storage.EntryStateDownloading, debridTypes.TorrentStatusDownloading,
				"torbox"
			e.Providers = map[string]*storage.ProviderEntry{
				"torbox": {Provider: "torbox", ID: "t1", Status: debridTypes.TorrentStatusDownloading},
			}
			return e
		}, grab.ChildPoll},
		{"downloaded", func() *storage.Entry {
			e := testEntry("a")
			e.State, e.Status, e.ActiveProvider = storage.EntryStateDownloading, debridTypes.TorrentStatusDownloaded,
				"torbox"
			e.Providers = map[string]*storage.ProviderEntry{
				"torbox": {Provider: "torbox", ID: "t1", Status: debridTypes.TorrentStatusDownloaded},
			}
			return e
		}, grab.ChildAction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := newActorHarness(t, store)
				e := tc.entry()
				if len(e.Grab) != 0 {
					t.Fatal("a legacy record carries no grab document")
				}
				if err := store.UpdateQueuedGrab(e); err != nil {
					t.Fatal(err)
				}
				recoverAll(h)
				h.next(tc.child)
			})
		})
	}
}

func TestRecoverAfterLibraryPublishedButQueueStillActing(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		e := testEntry("a")
		seed(t, store, e, grab.State{Phase: grab.PhaseActing})
		if err := store.PublishEntry(e.Clone()); err != nil {
			t.Fatal(err)
		}
		recoverAll(h)

		reply(h.next(grab.ChildAction), grab.Event{Kind: grab.EvActionDone})
		synctest.Wait()
		if got := h.diskPhase(); got != grab.PhaseComplete {
			t.Fatalf("disk phase %v after the action, want complete", got)
		}
		if _, err := store.Get("a"); err != nil {
			t.Fatalf("library entry after completing: %v", err)
		}
	})
}

func TestRecoverOfDeletingRunsCleanup(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seed(t, store, testEntry("a"), phaseState(grab.PhaseDeleting, false))
		recoverAll(h)

		run := h.next(grab.ChildCleanup)
		if !run.call.cleanup.DeleteFiles {
			t.Fatalf("cleanup = %+v, want the cleanup the delete asked for", run.call.cleanup)
		}
		reply(run, grab.Event{Kind: grab.EvCleanupDone})
		synctest.Wait()
		if _, err := store.GetQueuedGrab("a"); !errors.Is(err, appendstore.ErrKeyNotFound) {
			t.Fatalf("queue record after the cleanup: %v, want removed", err)
		}
	})
}

func TestStopMidChildLeavesAResumableRecord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		phase grab.Phase
		first grab.ChildKind
		again grab.ChildKind
	}{
		{"submit", grab.PhaseSubmitting, grab.ChildSubmit, grab.ChildLookup},
		{"poll", grab.PhaseProvider, grab.ChildPoll, grab.ChildPoll},
		{"action", grab.PhaseActing, grab.ChildAction, grab.ChildAction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := newActorHarness(t, store)
				seedPhase(t, store, "a", grab.PhaseQueued)
				if tc.phase != grab.PhaseSubmitting {
					seedPhase(t, store, "a", tc.phase)
				}
				recoverAll(h)
				h.next(tc.first)

				h.stop()
				if got := h.diskPhase(); got != tc.phase {
					t.Fatalf("disk phase %v after a stop mid-child, want %v", got, tc.phase)
				}

				next := newActorHarness(t, store)
				recoverAll(next)
				next.next(tc.again)
			})
		})
	}
}

func TestBootWithMoreRecordsThanTheMailboxRecoversAll(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		var logs bytes.Buffer
		m := h.manager()
		m.logger = zerolog.New(&logs)
		total := entryMailboxSize * 3
		for i := range total {
			seedPhase(t, store, fmt.Sprintf("hash-%03d", i), grab.PhaseProvider)
		}
		records, packOf := m.readQueue()
		m.recoverEntries(records, packOf)
		synctest.Wait()

		polls := len(h.started)
		if refused := strings.Contains(logs.String(), "Some grabs were not recovered"); !refused && polls != total {
			t.Fatalf("recovered %d of %d grabs and logged no refusal", polls, total)
		}
		if polls != total {
			t.Fatalf("recovered %d of %d grabs; the runner has room for every one", polls, total)
		}
	})
}
