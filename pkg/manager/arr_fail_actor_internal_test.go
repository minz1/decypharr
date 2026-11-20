package manager

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type tellLog struct {
	mu      sync.Mutex
	told    []grab.Grab
	syncsAt []int64
	untold  []time.Time
	keys    []string
}

func (l *tellLog) tell(queue *countingQueue) func(grab.Grab) {
	return func(g grab.Grab) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.told = append(l.told, g)
		l.syncsAt = append(l.syncsAt, queue.syncs.Load())
	}
}

func (l *tellLog) untell(key string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	l.untold = append(l.untold, at)
}

func dropAsBadRelease(h *actorHarness) {
	h.t.Helper()
	h.mustAsk("a", grab.Event{Kind: grab.EvProviderDropped, Fail: grab.FailBadRelease, Reason: "bad"})
}

func TestActorResolvesArrIntoFail(t *testing.T) {
	t.Parallel()
	store, queue := newCountingStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		log := &tellLog{}
		h.a.failArr = func(grab.Grab) string { return "sonarr" }
		h.a.tell = log.tell(queue)
		seed(t, store, testEntry("a"), grab.State{Phase: grab.PhaseComplete})
		before := queue.syncs.Load()
		dropAsBadRelease(h)
		h.mustBe("a", grab.PhaseFailed)

		disk, _ := h.diskGrab("a")
		if want := (grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"}); disk.State.ArrFail != want {
			t.Fatalf("arr_fail %+v, want %+v", disk.State.ArrFail, want)
		}
		if syncs := queue.syncs.Load() - before; syncs != 1 {
			t.Fatalf("queue syncs = %d, want 1", syncs)
		}
		if len(log.told) != 1 || log.syncsAt[0] != before+1 || log.told[0].State.ArrFail != disk.State.ArrFail {
			t.Fatalf("told %d times at syncs %v with %+v; want once, after the sync, with the saved state",
				len(log.told), log.syncsAt, log.told)
		}
	})
}

func TestActorLeavesAnUnresolvedFailAlone(t *testing.T) {
	t.Parallel()
	store, queue := newCountingStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		log := &tellLog{}
		h.a.tell = log.tell(queue)
		seed(t, store, testEntry("a"), grab.State{Phase: grab.PhaseComplete})
		before := queue.syncs.Load()
		dropAsBadRelease(h)
		h.mustBe("a", grab.PhaseFailed)

		disk, _ := h.diskGrab("a")
		if disk.State.ArrFail != (grab.ArrFail{}) || queue.syncs.Load() != before || len(log.told) != 0 {
			t.Fatalf("arr_fail %+v, syncs %d, told %d; want none, no sync, no tell",
				disk.State.ArrFail, queue.syncs.Load()-before, len(log.told))
		}
	})
}

func TestManagerFailArrNamesOnlyAnArrTorrentGrab(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit func(*storage.Entry, *grab.State)
		want string
	}{
		"arr torrent":    {func(e *storage.Entry, _ *grab.State) { e.Protocol = config.ProtocolTorrent }, "sonarr"},
		"arr nzb":        {func(e *storage.Entry, _ *grab.State) { e.Protocol = config.ProtocolNZB }, ""},
		"empty protocol": {func(e *storage.Entry, _ *grab.State) { e.Protocol = "" }, ""},
		"unregistered": {func(e *storage.Entry, _ *grab.State) {
			e.Protocol, e.Category = config.ProtocolTorrent, "bindery"
		}, ""},
		"manual":       {func(e *storage.Entry, _ *grab.State) { e.Protocol, e.Manual = config.ProtocolTorrent, true }, ""},
		"season child": {func(e *storage.Entry, st *grab.State) { e.Protocol, st.PackKey = config.ProtocolTorrent, "pack" }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, entry := newDroppedTestManager(t, nil)
			st := grab.State{Phase: grab.PhaseFailed}
			tc.edit(entry, &st)
			if got := m.failArr(grab.Grab{Entry: entry, State: st}); got != tc.want {
				t.Fatalf("failArr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActorUntellsOnLeavingFailed(t *testing.T) {
	t.Parallel()
	for name, arrFail := range map[string]grab.ArrFail{
		"pending": {State: grab.ArrFailPending, Arr: "sonarr"},
		"queued":  {State: grab.ArrFailQueued, Arr: "sonarr", JobID: "job"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := deadlineHarness(t, store, noPoll)
				log := &tellLog{}
				h.a.untell = log.untell
				failed := failedWith(arrFail)
				readd(t, h, failed)

				if len(log.untold) != 1 || log.keys[0] != "a" || !log.untold[0].Equal(failed.Failure.At) {
					t.Fatalf("untell calls %v at %v, want one (a, %v)", log.keys, log.untold, failed.Failure.At)
				}
				disk, _ := h.diskGrab("a")
				if disk.State.Phase == grab.PhaseFailed || disk.State.ArrFail != (grab.ArrFail{}) {
					t.Fatalf("disk %v %+v, want left Failed, intent cleared", disk.State.Phase, disk.State.ArrFail)
				}
			})
		})
	}
}

func TestActorDoesNotUntellAFailedGrabWithoutAnIntent(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		log := &tellLog{}
		h.a.untell = log.untell
		readd(t, h, failedWith(grab.ArrFail{}))
		if len(log.untold) != 0 {
			t.Fatalf("untell calls %v, want none", log.keys)
		}
	})
}

func readd(t *testing.T, h *actorHarness, failed grab.State) {
	t.Helper()
	seed(t, h.store, testEntry("a"), failed)
	h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
	if err := h.manager().submitEntry(t.Context(), testEntry("a"), addReq()); err != nil {
		t.Fatal(err)
	}
	h.next(grab.ChildSubmit)
	synctest.Wait()
}

func TestRetentionKeepsPendingArrFail(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		arrFail grab.ArrFail
		want    bool
	}{
		"none":    {grab.ArrFail{}, true},
		"pending": {grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"}, false},
		"queued":  {grab.ArrFail{State: grab.ArrFailQueued, Arr: "sonarr", JobID: "job"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := deadlineHarness(t, store, time.Hour)
				h.a.arrOwnsFailure = func(grab.Grab) bool { return false }
				seed(t, store, testEntry("a"), failedWith(tc.arrFail))
				st := &entryState{g: grab.Grab{State: failedWith(tc.arrFail), Entry: testEntry("a")}}
				h.grabs.markSeen("a", st.g.State.Failure.At)
				if got := h.a.deadlineView("a", st).Retention; (got != 0) != tc.want {
					t.Fatalf("retention = %v, armed = %t, want armed = %t", got, got != 0, tc.want)
				}
			})
		})
	}
}
