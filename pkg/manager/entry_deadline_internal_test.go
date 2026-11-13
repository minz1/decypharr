package manager

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const noPoll = 1000 * time.Hour

func deadlineHarness(t *testing.T, store *storage.Storage, removeStalledAfter time.Duration) *actorHarness {
	t.Helper()
	h := newActorHarness(t, store)
	h.a.pollEvery = noPoll
	h.a.removeStalledAfter = removeStalledAfter
	return h
}

func (h *actorHarness) failsSeen() []grab.FailEffect {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.fails)
}

func (h *actorHarness) diskGrab(key string) (grab.Grab, bool) {
	h.t.Helper()
	e, err := h.store.GetQueuedGrab(key)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return grab.Grab{}, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	g, err := grab.Load(e, nil, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return g, true
}

func (h *actorHarness) mustBe(key string, want grab.Phase) {
	h.t.Helper()
	synctest.Wait()
	if got := h.phase(key); got != want {
		h.t.Fatalf("%s is %v, want %v", key, got, want)
	}
}

func (h *actorHarness) providerGrab() childRun {
	h.t.Helper()
	const key = "a"
	e := testEntry(key)
	e.AddedOn = time.Now()
	seed(h.t, h.store, e, grab.State{Phase: grab.PhaseProvider})
	h.mustAsk(key, grab.Event{Kind: grab.EvRecover})
	return h.next(grab.ChildPoll)
}

func TestAPendingProviderFailsAtItsLimit(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		seen  pollSeen
		limit time.Duration
		fail  grab.FailEffect
	}{
		"provider":   {pollSeen{pending: grab.PendingProvider}, 6 * time.Hour, grab.FailEntry},
		"dead":       {pollSeen{pending: grab.PendingProvider, dead: true}, 6 * time.Hour, grab.FailBadRelease},
		"converting": {pollSeen{pending: grab.PendingConverting, dead: true}, 2 * time.Hour, grab.FailBadRelease},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := deadlineHarness(t, store, 0)
				run := h.providerGrab()
				run.reply <- entryMsg{seen: &c.seen}
				h.mustBe("a", grab.PhaseProvider)

				time.Sleep(c.limit - time.Minute)
				h.mustBe("a", grab.PhaseProvider)
				time.Sleep(time.Minute)
				h.mustBe("a", grab.PhaseFailed)
				if got := h.failsSeen(); !slices.Equal(got, []grab.FailEffect{c.fail}) {
					t.Fatalf("fail effects %v, want %v", got, c.fail)
				}
				if g, _ := h.diskGrab("a"); g.State.Phase != grab.PhaseFailed {
					t.Fatalf("disk phase %v, want failed", g.State.Phase)
				}
			})
		})
	}
}

func TestAPlacementOfUnknownAgeNeverFailsPending(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		seed(t, store, testEntry("a"), grab.State{Phase: grab.PhaseProvider})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		run := h.next(grab.ChildPoll)
		run.reply <- entryMsg{seen: &pollSeen{pending: grab.PendingProvider}}
		synctest.Wait()

		time.Sleep(100 * time.Hour)
		h.mustBe("a", grab.PhaseProvider)
	})
}

func observed(stalled bool) entryMsg {
	return eventMsg(grab.Event{Kind: grab.EvProviderObserved, Stalled: stalled})
}

func TestAStalledPlacementFailsAfterTheLimitWithNoEffect(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.providerGrab()
		h.send(observed(true))
		g, _ := h.diskGrab("a")
		if g.State.StalledSince.IsZero() {
			t.Fatal("the first stalled poll did not save when the stall began")
		}

		time.Sleep(time.Hour - time.Minute)
		h.mustBe("a", grab.PhaseProvider)
		time.Sleep(time.Minute)
		h.mustBe("a", grab.PhaseFailed)
		if got := h.failsSeen(); !slices.Equal(got, []grab.FailEffect{grab.FailNoEffect}) {
			t.Fatalf("fail effects %v, want none", got)
		}
	})
}

func TestAPollThatIsNotStalledClearsTheStallAndItsDeadline(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.providerGrab()
		h.send(observed(true))
		time.Sleep(50 * time.Minute)
		h.send(observed(false))
		if g, _ := h.diskGrab("a"); !g.State.StalledSince.IsZero() {
			t.Fatal("a poll that was not stalled left the stall saved")
		}
		h.send(observed(true))

		time.Sleep(15 * time.Minute)
		h.mustBe("a", grab.PhaseProvider)
		time.Sleep(45 * time.Minute)
		h.mustBe("a", grab.PhaseFailed)
	})
}

func TestAStallWithoutALimitNeverFails(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.providerGrab()
		h.send(observed(true))
		time.Sleep(100 * time.Hour)
		h.mustBe("a", grab.PhaseProvider)
	})
}

func TestAPendingPollLeavesTheStallAlone(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.providerGrab()
		h.send(observed(true))
		want, _ := h.diskGrab("a")
		h.send(entryMsg{exit: true, child: grab.ChildPoll, seen: &pollSeen{pending: grab.PendingProvider}})
		got, _ := h.diskGrab("a")
		if !got.State.StalledSince.Equal(want.State.StalledSince) || got.State.StalledSince.IsZero() {
			t.Fatalf("stalled since %v after a pending poll, want %v", got.State.StalledSince, want.State.StalledSince)
		}
	})
}

func TestLinksNeverReadyFailTheGrabAtTheLimit(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		e := testEntry("a")
		downloaded := time.Now()
		e.Providers = map[string]*storage.ProviderEntry{
			"realdebrid": {Provider: "realdebrid", ID: "t1", DownloadedAt: &downloaded},
		}
		seed(t, store, e, grab.State{Phase: grab.PhaseAwaitingLinks})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildLinks)

		time.Sleep(6*time.Hour - time.Minute)
		h.mustBe("a", grab.PhaseAwaitingLinks)
		time.Sleep(time.Minute)
		h.mustBe("a", grab.PhaseFailed)
		if got := h.failsSeen(); !slices.Equal(got, []grab.FailEffect{grab.FailBadRelease}) {
			t.Fatalf("fail effects %v, want a bad release", got)
		}
	})
}

func (h *actorHarness) failedGrab(key string, at time.Time) {
	h.t.Helper()
	seed(h.t, h.store, testEntry(key), grab.State{
		Phase: grab.PhaseFailed, Failure: grab.Failure{Reason: "x", At: at, Count: 1},
	})
}

func (h *actorHarness) arrSees(key string) {
	h.t.Helper()
	e, err := h.store.GetQueued(key)
	if err != nil {
		h.t.Fatal(err)
	}
	h.manager().ArrSeen([]*storage.Entry{e})
	synctest.Wait()
}

func (h *actorHarness) removedAfterCleanup(key string) {
	h.t.Helper()
	run := h.next(grab.ChildCleanup)
	if run.call.cleanup != (grab.Cleanup{}) {
		h.t.Fatalf("retention cleanup %+v, want the zero cleanup", run.call.cleanup)
	}
	reply(run, grab.Event{Kind: grab.EvCleanupDone})
	synctest.Wait()
	if _, ok := h.diskGrab(key); ok {
		h.t.Fatal("the failed grab is still on disk")
	}
}

func TestAFailedGrabIsKeptUntilAnArrSawItThenRemovedAtItsRetention(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.failedGrab("a", time.Now().Add(500*time.Millisecond))
		h.send(entryMsg{deadline: true})

		time.Sleep(10 * time.Hour)
		h.mustBe("a", grab.PhaseFailed)
		h.none()

		h.failedGrab("b", time.Now().Add(500*time.Millisecond))
		h.arrSees("b")
		h.mustBe("b", grab.PhaseFailed)
		time.Sleep(time.Hour - time.Minute)
		h.mustBe("b", grab.PhaseFailed)
		time.Sleep(time.Minute + time.Second)
		h.removedAfterCleanup("b")
	})
}

func TestARetentionThatPassedBeforeTheListingRemovesTheGrabAtOnce(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.failedGrab("a", time.Now())
		time.Sleep(5 * time.Hour)
		h.arrSees("a")
		h.removedAfterCleanup("a")
	})
}

func TestAListingOfAnotherFailureIsIgnored(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		at := time.Now()
		h.failedGrab("a", at)
		if err := h.a.runner.Send("a", entryMsg{arrSaw: at.Add(-5 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := h.grabs.seenFailure("a"); !got.IsZero() {
			t.Fatalf("seen %v after a listing of another failure", got)
		}
		time.Sleep(10 * time.Hour)
		h.mustBe("a", grab.PhaseFailed)
	})
}

func TestALegacyFailureWithoutAnErrorTimeExpiresFromWhenItWasAdded(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		e := testEntry("a")
		e.State, e.LastError, e.AddedOn = storage.EntryStateError, "x", time.Now()
		if err := store.AddQueue(e); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Hour)
		h.arrSees("a")
		h.removedAfterCleanup("a")
	})
}

func TestTheRetentionTimerSurvivesTheActorGoingIdle(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.failedGrab("a", time.Now())
		h.arrSees("a")
		time.Sleep(30 * time.Minute)
		h.mustBe("a", grab.PhaseFailed)
		time.Sleep(30 * time.Minute)
		h.removedAfterCleanup("a")
	})
}

func TestARestartWaitsForANewListing(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		first := deadlineHarness(t, store, time.Hour)
		first.failedGrab("a", time.Now())
		first.arrSees("a")
		first.stop()

		h := deadlineHarness(t, store, time.Hour)
		h.send(entryMsg{deadline: true})
		time.Sleep(10 * time.Hour)
		h.mustBe("a", grab.PhaseFailed)
		h.arrSees("a")
		h.removedAfterCleanup("a")
	})
}

func TestAFailedGrabWithoutARetentionIsNeverRemoved(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.failedGrab("a", time.Now())
		h.arrSees("a")
		time.Sleep(1000 * time.Hour)
		h.mustBe("a", grab.PhaseFailed)
		h.none()
	})
}

func TestAFailedSeasonIsHeldWhileItsPackRunsAndWokenWhenThePackEnds(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.h.a.pollEvery = noPoll
		p.h.a.removeStalledAfter = time.Hour
		p.startPack()
		failed, other := p.keys[0], p.keys[1]
		reply(p.take(grab.ChildAction, failed), grab.Event{Kind: grab.EvActionFailed, Fail: grab.FailEntry})
		p.want(failed, grab.PhaseFailed)
		p.want("pack", grab.PhaseActing)
		at := time.Now()
		if got, _ := p.h.grabs.get(failed); got.State.Failure.At.IsZero() {
			t.Fatal("the season has no failure time")
		}
		if err := p.h.a.runner.Send(failed, entryMsg{arrSaw: at}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		time.Sleep(5 * time.Hour)
		p.want(failed, grab.PhaseFailed)

		p.finish(other)
		p.want("pack", grab.PhaseComplete)
		run := p.take(grab.ChildCleanup, failed)
		reply(run, grab.Event{Kind: grab.EvCleanupDone})
		synctest.Wait()
		if _, ok := p.h.grabs.get(failed); ok {
			t.Fatal("the failed season is still held after its pack ended")
		}
	})
}

type countingWriter struct {
	mu    sync.Mutex
	match string
	n     int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n += strings.Count(string(p), w.match)
	return len(p), nil
}

func (w *countingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

func TestADueDeadlineWhoseSaveFailsRetriesOnTheSaveBackoff(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		saves := &countingWriter{match: "Grab not saved"}
		h.a.logger = zerolog.New(saves)
		h.providerGrab()
		h.send(observed(true))
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := saves.count(); got == 0 || got > 20 {
			t.Fatalf("%d failed saves in the hour after the deadline, want a few on the save backoff", got)
		}
		h.mustBe("a", grab.PhaseProvider)
	})
}
