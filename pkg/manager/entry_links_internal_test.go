package manager

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func linksRef() placementRef { return placementRef{Provider: "realdebrid", TorrentID: "t1"} }

type pollAnswer struct {
	t   *debridTypes.Torrent
	obs debridTypes.Observation
	err error
}

type scriptedPoll struct {
	debrid.Client

	mu      sync.Mutex
	answers []pollAnswer
	deleted []string
}

func (p *scriptedPoll) DeleteTorrent(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, id)
	return nil
}

func (p *scriptedPoll) deletes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.deleted)
}

func (*scriptedPoll) Config() config.Debrid { return config.Debrid{Name: "realdebrid"} }

func (p *scriptedPoll) Poll(
	_ context.Context,
	t *debridTypes.Torrent,
	_ bool,
) (*debridTypes.Torrent, debridTypes.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.answers[0]
	if len(p.answers) > 1 {
		p.answers = p.answers[1:]
	}
	if a.t == nil && a.err == nil {
		a.t = t
	}
	return a.t, a.obs, a.err
}

func transientAnswer() pollAnswer { return pollAnswer{err: context.DeadlineExceeded} }

func pendingAnswer() pollAnswer {
	return pollAnswer{obs: debridTypes.Observation{Phase: debridTypes.PhaseFinalizing, LinksPending: true}}
}

func (h *actorHarness) linksProvider(t *testing.T, answers ...pollAnswer) (*Manager, *scriptedPoll) {
	t.Helper()
	m, _ := newDroppedTestManager(t, nil)
	m.store = testConfigStore(t, nil)
	client := &scriptedPoll{answers: answers}
	m.clients.Store("realdebrid", client)
	h.a.child = func(call entryChildCall) func(context.Context) entryMsg {
		return func(ctx context.Context) entryMsg { return m.pollChild(ctx, call, call.kind == grab.ChildLinks) }
	}
	h.a.pollEvery = time.Minute
	return m, client
}

func linksEntryAt(downloaded time.Duration) *storage.Entry {
	e := testEntry("a")
	at := time.Now().Add(-downloaded)
	e.Providers = map[string]*storage.ProviderEntry{
		"realdebrid": {Provider: "realdebrid", ID: "t1", DownloadedAt: &at},
	}
	return e
}

func (h *actorHarness) recoverLinksWait(downloaded time.Duration) {
	h.t.Helper()
	seed(h.t, h.store, linksEntryAt(downloaded), grab.State{Phase: grab.PhaseAwaitingLinks})
	h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
}

func (h *actorHarness) restoredLinksWait(downloaded time.Duration) childRun {
	h.t.Helper()
	h.recoverLinksWait(downloaded)
	return h.next(grab.ChildLinks)
}

func stillMissing() entryMsg {
	ref := linksRef()
	return entryMsg{linksMissing: &ref}
}

func (h *actorHarness) running() bool {
	for _, k := range h.a.runner.Snapshot().Keys {
		if k.Key == "a" {
			return k.Running
		}
	}
	return false
}

func (h *actorHarness) mustFailWith(want ...grab.FailEffect) {
	h.t.Helper()
	if got := h.failsSeen(); !slices.Equal(got, want) {
		h.t.Fatalf("fail effects %v, want %v", got, want)
	}
}

func TestARestoredLinksWaitWaitsForALinksAnswer(t *testing.T) {
	t.Parallel()
	t.Run("still missing", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := deadlineHarness(t, store, 0)
			run := h.restoredLinksWait(7 * time.Hour)
			time.Sleep(time.Hour)
			h.mustBe("a", grab.PhaseAwaitingLinks)
			h.mustFailWith()
			run.reply <- stillMissing()
			h.mustBe("a", grab.PhaseFailed)
			h.mustFailWith(grab.FailBadRelease)
		})
	})
	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := deadlineHarness(t, store, 0)
			run := h.restoredLinksWait(7 * time.Hour)
			time.Sleep(time.Hour)
			reply(run, grab.Event{Kind: grab.EvLinksReady})
			h.mustBe("a", grab.PhaseActing)
			h.mustFailWith()
		})
	})
	t.Run("a finalizing answer from the provider", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := deadlineHarness(t, store, 0)
			h.linksProvider(t, transientAnswer(), pendingAnswer())
			h.recoverLinksWait(7 * time.Hour)
			synctest.Wait()
			h.mustBe("a", grab.PhaseAwaitingLinks)
			time.Sleep(time.Minute)
			h.mustBe("a", grab.PhaseFailed)
			h.mustFailWith(grab.FailBadRelease)
		})
	})
}

func TestARestoredLinksWaitWithOnlyTransientPollsEndsAtTheBound(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.linksProvider(t, transientAnswer())
		h.recoverLinksWait(7 * time.Hour)
		time.Sleep(6*time.Hour - time.Second)
		h.mustBe("a", grab.PhaseAwaitingLinks)
		h.mustFailWith()
		time.Sleep(time.Second)
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(grab.FailBadRelease)
	})
}

func TestARestoredLinksWaitWithOnlyUnconfirmed404sEndsAtTheBound(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.linksProvider(t, pollAnswer{err: unconfirmed404()})
		h.recoverLinksWait(7 * time.Hour)
		time.Sleep(6*time.Hour - time.Second)
		h.mustBe("a", grab.PhaseAwaitingLinks)
		h.mustFailWith()
		if mark, ok := h.grabs.linksOf("a"); !ok || mark.fresh {
			t.Fatalf("mark %+v (%v), want an unanswered hold", mark, ok)
		}
		time.Sleep(time.Second)
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(grab.FailBadRelease)
	})
}

func TestAHardFailureWhileHeldDecidesAsInProcessHEAD(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		hard := customerror.NewProviderStatusError(errors.New("realdebrid API error: Status: 401"), http.StatusUnauthorized)
		m, client := h.linksProvider(t, transientAnswer(),
			pollAnswer{t: &debridTypes.Torrent{ID: "t1"}, err: hard})
		h.recoverLinksWait(7 * time.Hour)
		synctest.Wait()
		h.mustBe("a", grab.PhaseAwaitingLinks)
		h.mustFailWith()
		if got := client.deletes(); len(got) != 0 {
			t.Fatalf("deleted %v while only a transient poll had answered", got)
		}
		time.Sleep(time.Minute)
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(grab.FailEntry)
		drainPlacements(t, m)
		if got := client.deletes(); !slices.Equal(got, []string{"t1"}) {
			t.Fatalf("deleted %v, want [t1]", got)
		}
	})
}

func TestAFailedExitSaveKeepsTheLinksBound(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, fresh bool) {
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := deadlineHarness(t, store, 0)
			var failed atomic.Bool
			h.a.publish = func(*storage.Entry) error {
				if !failed.Swap(true) {
					return errors.New("disk")
				}
				return nil
			}
			started := time.Now()
			downloaded, wait := 7*time.Hour, 5*time.Hour
			if fresh {
				downloaded, wait = time.Hour, 4*time.Hour
			}
			child := h.restoredLinksWait(downloaded)
			if fresh {
				child.reply <- stillMissing()
				synctest.Wait()
			}
			time.Sleep(wait)
			h.send(eventMsg(grab.Event{Kind: grab.EvLinksReady}))
			if !failed.Load() {
				t.Fatal("the exit save did not fail")
			}
			h.send(entryMsg{ev: &grab.Event{Kind: grab.EvTagsChanged}, tags: []string{"x"}})
			mark, ok := h.grabs.linksOf("a")
			if !ok || mark.fresh != fresh || !mark.heldSince.Equal(started) {
				t.Fatalf("mark %+v (%v), want fresh %v held since %v", mark, ok, fresh, started)
			}
			if fresh {
				return
			}
			time.Sleep(time.Hour - time.Second)
			h.mustBe("a", grab.PhaseAwaitingLinks)
			time.Sleep(time.Second)
			h.mustBe("a", grab.PhaseFailed)
		})
	}
	t.Run("unfresh", func(t *testing.T) { t.Parallel(); run(t, false) })
	t.Run("fresh", func(t *testing.T) { t.Parallel(); run(t, true) })
}

func TestAStaleDownloadedAtBlocklistsAtOnceInProcess(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		seed(t, store, linksEntryAt(7*time.Hour), grab.State{Phase: grab.PhaseProvider})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		run := h.next(grab.ChildPoll)
		run.reply <- eventMsg(grab.Event{
			Kind: grab.EvProviderObserved, Observed: debridTypes.PhaseDownloaded, Downloaded: true, AllLinked: false,
		})
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(grab.FailBadRelease)
	})
}

func TestAnInProcessSubmitIntoAwaitingLinksFailsAtHEADsTime(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		submit := h.submitting("a")
		msg := eventMsg(grab.Event{
			Kind: grab.EvSubmitResult, Outcome: grab.OutcomePlacement, Provider: "realdebrid", TorrentID: "t1",
			Downloaded: true, AllLinked: false,
		})
		msg.torrent = &debridTypes.Torrent{ID: "t1", Debrid: "realdebrid", Status: debridTypes.TorrentStatusDownloaded}
		msg.update = func(e *storage.Entry) {
			at := time.Now().Add(-time.Hour)
			e.Providers["realdebrid"].DownloadedAt = &at
		}
		submit.reply <- msg
		h.mustBe("a", grab.PhaseAwaitingLinks)
		mark, ok := h.grabs.linksOf("a")
		if !ok || !mark.fresh || mark.placement != linksRef() {
			t.Fatalf("mark %+v (%v), want fresh for %+v", mark, ok, linksRef())
		}
		time.Sleep(5*time.Hour - time.Second)
		h.mustBe("a", grab.PhaseAwaitingLinks)
		time.Sleep(time.Second)
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(grab.FailBadRelease)
	})
}

func TestALinksMarkForAnotherPlacementDoesNotVouch(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.grabs.setLinks("a", linksMark{
			placement: placementRef{Provider: "realdebrid", TorrentID: "other"}, fresh: true,
			heldSince: time.Now().Add(-10 * time.Hour),
		})
		h.restoredLinksWait(7 * time.Hour)
		time.Sleep(6*time.Hour - time.Second)
		h.mustBe("a", grab.PhaseAwaitingLinks)
		time.Sleep(time.Second)
		h.mustBe("a", grab.PhaseFailed)
	})
}

func TestALinksAnswerForTheOldPlacementDoesNotVouch(t *testing.T) {
	t.Parallel()
	for name, old := range map[string]placementRef{
		"another torrent":  {Provider: "realdebrid", TorrentID: "t0"},
		"another provider": {Provider: "torbox", TorrentID: "t1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := deadlineHarness(t, store, 0)
				h.restoredLinksWait(7 * time.Hour)
				h.send(entryMsg{exit: true, child: grab.ChildLinks, linksMissing: &old})
				time.Sleep(6*time.Hour - time.Second)
				h.mustBe("a", grab.PhaseAwaitingLinks)
				time.Sleep(time.Second)
				h.mustBe("a", grab.PhaseFailed)
			})
		})
	}
}

func TestTheLinksBoundSurvivesIdleExit(t *testing.T) {
	t.Parallel()
	t.Run("unfresh", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := deadlineHarness(t, store, 0)
			run := h.restoredLinksWait(7 * time.Hour)
			run.reply <- entryMsg{}
			time.Sleep(2 * time.Hour)
			synctest.Wait()
			if h.running() {
				t.Fatal("the actor is still up after two idle hours")
			}
			h.mustBe("a", grab.PhaseAwaitingLinks)
			time.Sleep(4*time.Hour - time.Second)
			h.mustBe("a", grab.PhaseAwaitingLinks)
			time.Sleep(time.Second)
			h.mustBe("a", grab.PhaseFailed)
		})
	})
	t.Run("fresh", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := deadlineHarness(t, store, 0)
			run := h.restoredLinksWait(time.Hour)
			run.reply <- stillMissing()
			time.Sleep(2 * time.Hour)
			synctest.Wait()
			if h.running() {
				t.Fatal("the actor is still up after two idle hours")
			}
			time.Sleep(3*time.Hour - time.Second)
			h.mustBe("a", grab.PhaseAwaitingLinks)
			time.Sleep(time.Second)
			h.mustBe("a", grab.PhaseFailed)
		})
	})
}

func TestRemovingAGrabForgetsItsLinksMark(t *testing.T) {
	t.Parallel()
	x := &grabIndex{}
	x.setLinks("A", linksMark{fresh: true})
	x.remove("a")
	if _, ok := x.linksOf("a"); ok {
		t.Fatal("the mark outlived the grab")
	}
}

func TestALinksCheckReportsStillMissingOnlyOnLinksPending(t *testing.T) {
	t.Parallel()
	entry := func() *storage.Entry {
		e := linksEntryAt(time.Hour)
		e.ActiveProvider = "realdebrid"
		return e
	}
	finalizing := debridTypes.Observation{Phase: debridTypes.PhaseFinalizing}
	downloaded := debridTypes.Observation{Phase: debridTypes.PhaseDownloaded}
	unlinked := &debridTypes.Torrent{ID: "t1", Debrid: "realdebrid", Status: debridTypes.TorrentStatusDownloaded,
		Files: map[string]debridTypes.File{"a.mkv": {Name: "a.mkv"}}}
	pendingWithError := pendingAnswer()
	pendingWithError.err = context.DeadlineExceeded
	cases := []struct {
		name   string
		answer pollAnswer
		want   bool
		wantEv grab.EventKind
		hasEv  bool
	}{
		{name: "links pending", answer: pendingAnswer(), want: true},
		{name: "downloaded with unlinked files", answer: pollAnswer{t: unlinked, obs: downloaded}, want: true},
		{name: "finalizing without links pending", answer: pollAnswer{obs: finalizing}},
		{name: "a transient error", answer: transientAnswer()},
		{name: "links pending with an error", answer: pendingWithError},
		{name: "a confirmed absence", answer: pollAnswer{err: customerror.ErrTorrentNotFound}, hasEv: true,
			wantEv: grab.EvProviderObserved},
		{name: "an unconfirmed 404", answer: pollAnswer{err: unconfirmed404()}},
		{name: "downloaded without files", answer: pollAnswer{obs: downloaded}, hasEv: true,
			wantEv: grab.EvLinksReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newDroppedTestManager(t, nil)
			m.store = testConfigStore(t, nil)
			m.clients.Store("realdebrid", &scriptedPoll{answers: []pollAnswer{c.answer}})
			e := entry()
			msg := m.pollChild(t.Context(), entryChildCall{kind: grab.ChildLinks, grab: grab.Grab{Entry: e}}, true)
			if c.want {
				if msg.linksMissing == nil || *msg.linksMissing != linksRef() {
					t.Fatalf("linksMissing %v, want %+v", msg.linksMissing, linksRef())
				}
				return
			}
			if msg.linksMissing != nil {
				t.Fatalf("linksMissing %+v, want none", msg.linksMissing)
			}
			if c.hasEv && (msg.ev == nil || msg.ev.Kind != c.wantEv) {
				t.Fatalf("event %+v, want %v", msg.ev, c.wantEv)
			}
		})
	}
}

func (h *actorHarness) deadline() *grab.Gated {
	h.t.Helper()
	v, ok := h.a.grabs.get("a")
	if !ok {
		h.t.Fatal("no view")
	}
	return v.Deadline
}

func TestTheGatedDeadlineReachesTheIndex(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		start := time.Now()
		run := h.restoredLinksWait(0)
		synctest.Wait()
		d := h.deadline()
		if d == nil || !d.WaitingForLinks || d.Event.Kind != grab.EvLinkWaitExpired ||
			!d.Due.Equal(start.Add(grab.LinksLimit)) {
			t.Fatalf("restored wait deadline = %+v, want waiting for links due at the limit", d)
		}
		run.reply <- stillMissing()
		synctest.Wait()
		d = h.deadline()
		if d == nil || d.WaitingForLinks || !d.Due.Equal(start.Add(grab.LinksLimit)) {
			t.Fatalf("answered wait deadline = %+v, want not waiting, same due", d)
		}
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if h.running() {
			t.Fatal("the actor is still up after two idle hours")
		}
		if d = h.deadline(); d == nil || d.WaitingForLinks || !d.Due.Equal(start.Add(grab.LinksLimit)) {
			t.Fatalf("idle deadline = %+v, want the answered deadline kept", d)
		}
		time.Sleep(4 * time.Hour)
		h.mustBe("a", grab.PhaseFailed)
		if d = h.deadline(); d != nil && d.Event.Kind == grab.EvLinkWaitExpired {
			t.Fatalf("failed grab still shows the links deadline %+v", d)
		}
	})
}

func TestADeadlineIsNilWhenTheGrabHasNone(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		seed(t, store, linksEntryAt(0), grab.State{Phase: grab.PhaseComplete})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		if d := h.deadline(); d != nil {
			t.Fatalf("complete grab deadline = %+v, want none", d)
		}
	})
}

func TestAnAddedViewDoesNotInheritAnotherPhasesDeadline(t *testing.T) {
	t.Parallel()
	x := &grabIndex{}
	v := GrabView{State: grab.State{Key: "K", Phase: grab.PhaseAwaitingLinks}}
	x.publish(v)
	x.setDeadline("k", &grab.Gated{WaitingForLinks: true})
	x.publish(v)
	if got, _ := x.get("k"); got.Deadline == nil || !got.Deadline.WaitingForLinks {
		t.Fatalf("same-phase publish dropped the deadline: %+v", got.Deadline)
	}
	v.State.Phase = grab.PhaseFailed
	v.Deadline = &grab.Gated{}
	x.publish(v)
	if got, _ := x.get("k"); got.Deadline != nil {
		t.Fatalf("phase change kept the deadline: %+v", got.Deadline)
	}
	x.remove("k")
	x.setDeadline("k", &grab.Gated{})
	if _, ok := x.get("k"); ok {
		t.Fatal("setDeadline resurrected a removed grab")
	}
}

func TestAReadersViewDoesNotShareTheIndexsDeadline(t *testing.T) {
	t.Parallel()
	x := &grabIndex{}
	x.publish(GrabView{State: grab.State{Key: "K", Phase: grab.PhaseAwaitingLinks}})
	x.setDeadline("k", &grab.Gated{WaitingForLinks: true})
	got, _ := x.get("k")
	got.Deadline.WaitingForLinks = false
	for _, v := range x.snapshot() {
		v.Deadline.WaitingForLinks = false
	}
	if again, _ := x.get("k"); again.Deadline == nil || !again.Deadline.WaitingForLinks {
		t.Fatalf("a reader's write reached the index: %+v", again.Deadline)
	}
}

func TestAnOrderedLateAnswerForTheFirstPlacementDoesNotVouchForTheSecond(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.restoredLinksWait(7 * time.Hour)
		h.send(eventMsg(grab.Event{
			Kind: grab.EvProviderObserved, Observed: debridTypes.PhaseFailed, Fail: grab.FailNoEffect, Reason: "gone",
		}))
		h.mustBe("a", grab.PhaseFailed)
		h.send(eventMsg(grab.Event{Kind: grab.EvFailedRetention}))
		h.next(grab.ChildCleanup).reply <- eventMsg(grab.Event{Kind: grab.EvCleanupDone})
		h.mustBe("a", grab.PhaseAbsent)
		time.Sleep(time.Hour)
		synctest.Wait()
		if h.running() {
			t.Fatal("the actor is still up an hour after the grab was removed")
		}
		second := linksEntryAt(7 * time.Hour)
		second.Providers["realdebrid"].ID = "t2"
		second.ActiveProvider = "realdebrid"
		seed(t, store, second, grab.State{Phase: grab.PhaseAwaitingLinks})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildLinks)
		first := linksRef()
		h.send(entryMsg{exit: true, child: grab.ChildLinks, linksMissing: &first})
		mark, ok := h.grabs.linksOf("a")
		want := placementRef{Provider: "realdebrid", TorrentID: "t2"}
		if !ok || mark.fresh || mark.placement != want {
			t.Fatalf("mark %+v (%v), want an unanswered hold of %+v", mark, ok, want)
		}
		time.Sleep(6*time.Hour - time.Second)
		h.mustBe("a", grab.PhaseAwaitingLinks)
		time.Sleep(time.Second)
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(grab.FailNoEffect, grab.FailBadRelease)
	})
}

func TestAWaitForLinksWithNoPlacementRecordWritesNoMark(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		e := testEntry("a")
		e.ActiveProvider = "ghost"
		st := grab.State{Key: "a", Phase: grab.PhaseAwaitingLinks, PhaseSince: time.Now()}
		if err := grab.Encode(grab.Grab{State: st, Entry: e}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateQueuedGrab(e); err != nil {
			t.Fatal(err)
		}
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildLinks)
		if mark, ok := h.grabs.linksOf("a"); ok {
			t.Fatalf("mark %+v written for a grab with no placement", mark)
		}
		if d := h.deadline(); d != nil {
			t.Fatalf("deadline %+v for a grab with no placement", d)
		}
	})
}
