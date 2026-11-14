package manager

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func listedKeys(views []GrabView) []string {
	keys := make([]string, 0, len(views))
	for _, v := range views {
		keys = append(keys, v.State.Key)
	}
	return keys
}

func TestQBitFilterNames(t *testing.T) {
	t.Parallel()
	all := []grab.Phase{
		grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseProvider, grab.PhaseAwaitingLinks,
		grab.PhaseActing, grab.PhaseComplete, grab.PhaseFailed,
	}
	cases := map[string][]grab.Phase{
		"all":     nil,
		"unknown": nil,
		"":        nil,
		"downloading": {
			grab.PhaseQueued,
			grab.PhaseThrottled,
			grab.PhaseSubmitting,
			grab.PhaseProvider,
			grab.PhaseAwaitingLinks,
			grab.PhaseActing,
		},
		"completed": {grab.PhaseComplete},
		"paused":    {grab.PhaseComplete},
		"active":    {grab.PhaseProvider, grab.PhaseAwaitingLinks, grab.PhaseActing},
		"inactive": {
			grab.PhaseQueued,
			grab.PhaseThrottled,
			grab.PhaseSubmitting,
			grab.PhaseComplete,
			grab.PhaseFailed,
		},
		"errored": {grab.PhaseFailed},
		"seeding": {},
		"stalled": {},
	}
	for name, want := range cases {
		got := QBitFilter(name)
		if (got == nil) != (want == nil) || !slices.Equal(got, want) {
			t.Errorf("QBitFilter(%q) = %v, want %v", name, got, want)
		}
	}
	for _, p := range all {
		if !slices.Contains(QBitFilter("downloading"), p) && !slices.Contains(QBitFilter("completed"), p) &&
			!slices.Contains(QBitFilter("errored"), p) {
			t.Errorf("%v is in none of downloading, completed or errored", p)
		}
	}
}

func TestLegacyDerivedFailedIsSeenAndExpires(t *testing.T) {
	t.Parallel()
	legacy := map[string]func(e *storage.Entry){
		"unreadable": func(e *storage.Entry) {
			e.State, e.Status = storage.EntryStateDownloading, "unknown"
		},
		"missing-library": func(e *storage.Entry) {
			e.State, e.IsComplete = storage.EntryStatePausedUP, true
		},
		"orphan-season": func(*storage.Entry) {},
		"error-state": func(e *storage.Entry) {
			e.State, e.LastError = storage.EntryStateError, "refused"
		},
	}
	for name, shape := range legacy {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := deadlineHarness(t, store, time.Hour)
				e := testEntry("old")
				e.AddedOn = time.Now()
				shape(e)
				if err := store.UpdateQueue(e); err != nil {
					t.Fatal(err)
				}
				recoverAll(h)
				q := newQueue(store, nil, h.grabs, zerolog.Nop())
				views := q.ListFilter(ListQuery{Phases: []grab.Phase{grab.PhaseFailed}}, "", false)
				if keys := listedKeys(views); !slices.Equal(keys, []string{"old"}) {
					t.Fatalf("failed grabs listed %v, want [old]", keys)
				}
				h.manager().ArrSeen(views)
				synctest.Wait()
				time.Sleep(time.Hour + time.Second)
				h.removedAfterCleanup("old")
			})
		})
	}
}

func TestListFilterOrderIsStable(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		at := time.Now()
		for _, key := range []string{"c", "a", "b"} {
			e := testEntry(key)
			e.AddedOn = at
			seed(t, store, e, grab.State{Phase: grab.PhaseQueued})
		}
		recoverAll(h)
		q := newQueue(store, nil, h.grabs, zerolog.Nop())
		for range 5 {
			if keys := listedKeys(q.ListFilter(ListQuery{}, "", false)); !slices.Equal(keys, []string{"a", "b", "c"}) {
				t.Fatalf("listed %v, want [a b c]", keys)
			}
		}
		if got := q.ListFilter(ListQuery{Phases: []grab.Phase{}}, "", false); len(got) != 0 {
			t.Errorf("an empty phase set listed %v", listedKeys(got))
		}
		if got := q.ListFilter(ListQuery{Protocol: config.ProtocolNZB}, "", false); len(got) != 0 {
			t.Errorf("protocol nzb listed %v", listedKeys(got))
		}
	})
}

func TestListFilterIncludesUnreadableDocument(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		e := testEntry("bad")
		e.State, e.Status = storage.EntryStateDownloading, storage.EntryStatusQueued
		e.AddedOn = time.Now()
		e.Grab = []byte("{")
		if err := store.UpdateQueuedGrab(e); err != nil {
			t.Fatal(err)
		}
		recoverAll(h)
		q := newQueue(store, nil, h.grabs, zerolog.Nop())
		views := q.ListFilter(ListQuery{}, "", false)
		if len(views) != 1 || views[0].State.Key != "bad" {
			t.Fatalf("listed %v, want [bad]", listedKeys(views))
		}
		want := grab.FromLegacy(e, nil, false).Phase
		if views[0].State.Phase != want {
			t.Errorf("phase %v, want the legacy tuple's %v", views[0].State.Phase, want)
		}
	})
}

func TestListFilterSeesInMemoryProgress(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		e := testEntry("p")
		e.Progress = 0.1
		seed(t, store, e, grab.State{Phase: grab.PhaseProvider})
		recoverAll(h)
		v, ok := h.grabs.get("p")
		if !ok {
			t.Fatal("p is not in the index")
		}
		v.Entry.Progress = 0.5
		h.grabs.publish(v)
		q := newQueue(store, nil, h.grabs, zerolog.Nop())
		views := q.ListFilter(ListQuery{}, "", false)
		if len(views) != 1 || views[0].Entry.Progress != 0.5 {
			t.Fatalf("listed %+v, want progress 0.5", views)
		}
	})
}
