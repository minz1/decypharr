package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
	"github.com/sirrobot01/decypharr/internal/task"
)

func TestHolderTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, holderSuccessors, allHolderPhases(), []statetest.Edge[holderPhase]{
		{From: holderIdle, To: holderHeld},
		{From: holderIdle, To: holderDeleting},
		{From: holderHeld, To: holderIdle},
		{From: holderHeld, To: holderDeleting},
		{From: holderDeleting, To: holderIdle},
		{From: holderDeleting, To: holderHeld},
		{From: holderDeleting, To: holderGone},
		{From: holderGone, To: holderIdle},
	})
}

func stepAll(t *testing.T, st holderState, evs ...holderEvent) (holderState, []holderEffect) {
	t.Helper()
	var all []holderEffect
	for _, ev := range evs {
		var fx []holderEffect
		var err error
		if st, fx, err = holderStep(st, ev); err != nil {
			t.Fatalf("%v: %v", ev.kind, err)
		}
		all = append(all, fx...)
	}
	return st, all
}

func effectsOf(fx []holderEffect, kind holderEffectKind) []holderEffect {
	var out []holderEffect
	for _, e := range fx {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func hold(g string) holderEvent { return holderEvent{kind: holderHold, grab: g} }

func release(g string, del bool, done chan releaseResult) holderEvent {
	return holderEvent{kind: holderRelease, grab: g, del: del, done: done}
}

func ended(outcome releaseOutcome, stale ...string) holderEvent {
	return holderEvent{kind: holderDeleteDone, outcome: outcome, stale: stale}
}

func TestReleaseWithHoldsEmptyStartsTheDelete(t *testing.T) {
	t.Parallel()
	st, fx := stepAll(t, holderState{}, hold("a"), release("a", true, nil))
	if st.phase != holderDeleting || len(effectsOf(fx, fxHolderDelete)) != 1 {
		t.Fatalf("phase %v, deletes %d; want deleting and one delete", st.phase, len(effectsOf(fx, fxHolderDelete)))
	}
}

func TestReleaseWithoutDeleteNeverStartsADelete(t *testing.T) {
	t.Parallel()
	st, fx := stepAll(t, holderState{}, hold("a"), hold("b"), release("a", false, nil), release("b", false, nil))
	if st.phase != holderIdle || len(effectsOf(fx, fxHolderDelete)) != 0 {
		t.Fatalf("phase %v, deletes %d; want idle and none", st.phase, len(effectsOf(fx, fxHolderDelete)))
	}
}

func TestReleaseWithOtherHoldsVerifiesBeforeDeleting(t *testing.T) {
	t.Parallel()
	st, fx := stepAll(t, holderState{}, hold("a"), hold("b"), release("a", true, nil))
	deletes := effectsOf(fx, fxHolderDelete)
	if st.phase != holderDeleting || len(deletes) != 1 || !slices.Equal(deletes[0].holds, []string{"b"}) {
		t.Fatalf("phase %v, deletes %+v; want a verify carrying b's hold", st.phase, deletes)
	}
	st, _ = stepAll(t, st, ended(releaseHeldElsewhere))
	if st.phase != holderHeld {
		t.Fatalf("phase %v after held elsewhere, want held", st.phase)
	}
}

func TestStaleHoldsDropAndTheDeleteGoesAhead(t *testing.T) {
	t.Parallel()
	st, _ := stepAll(t, holderState{}, hold("a"), hold("b"), release("a", true, nil))
	st, _ = stepAll(t, st, ended(releaseHeldElsewhere, "b"))
	if st.phase != holderIdle || len(st.holds) != 0 {
		t.Fatalf("phase %v holds %v, want idle with the stale hold dropped", st.phase, st.holds)
	}
}

func TestWaitersAreAnsweredPerOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		outcome releaseOutcome
		phase   holderPhase
		want    releaseOutcome
	}{
		{releaseRemoved, holderGone, releaseRemoved},
		{releaseHeldElsewhere, holderIdle, releaseHeldElsewhere},
		{releaseStopped, holderIdle, releaseStopped},
		{releaseFailed, holderIdle, releaseFailed},
	}
	for _, c := range cases {
		t.Run(c.outcome.String(), func(t *testing.T) {
			t.Parallel()
			done := make(chan releaseResult, 1)
			st, fx := stepAll(t, holderState{}, hold("a"), release("a", true, done), ended(c.outcome))
			if st.phase != c.phase {
				t.Fatalf("phase %v, want %v", st.phase, c.phase)
			}
			answers := effectsOf(fx, fxHolderAnswer)
			if len(answers) != 1 || answers[0].result.outcome != c.want {
				t.Fatalf("answers %+v, want one %v", answers, c.want)
			}
		})
	}
}

func TestADeleteCutByTheStopReturnsToHeld(t *testing.T) {
	t.Parallel()
	st, _ := stepAll(t, holderState{}, hold("a"), hold("b"), release("a", true, nil), ended(releaseStopped))
	if st.phase != holderHeld || !slices.Equal(sortedHolds(st.holds), []string{"b"}) {
		t.Fatalf("phase %v holds %v, want held by b", st.phase, st.holds)
	}
}

func TestAReleaseWhileDeletingJoinsTheWaiters(t *testing.T) {
	t.Parallel()
	first, second := make(chan releaseResult, 1), make(chan releaseResult, 1)
	st, fx := stepAll(t, holderState{}, hold("a"), hold("b"),
		release("a", true, first), release("b", true, second), ended(releaseRemoved))
	if st.phase != holderGone || len(effectsOf(fx, fxHolderAnswer)) != 2 || len(effectsOf(fx, fxHolderDelete)) != 1 {
		t.Fatalf("phase %v answers %d deletes %d; want gone, two answers, one delete",
			st.phase, len(effectsOf(fx, fxHolderAnswer)), len(effectsOf(fx, fxHolderDelete)))
	}
}

func TestAJoinedReleaseFromAnotherGrabGetsItsOwnRecount(t *testing.T) {
	t.Parallel()
	for _, waiting := range []bool{true, false} {
		t.Run(fmt.Sprint("waiting=", waiting), func(t *testing.T) {
			t.Parallel()
			var first, second chan releaseResult
			if waiting {
				first, second = make(chan releaseResult, 1), make(chan releaseResult, 1)
			}
			st, _ := stepAll(t, holderState{}, hold("a"), hold("b"),
				release("a", true, first), release("b", true, second))
			st, fx := stepAll(t, st, ended(releaseHeldElsewhere))
			deletes := effectsOf(fx, fxHolderDelete)
			if st.phase != holderDeleting || len(deletes) != 1 || deletes[0].call.releasing != "b" {
				t.Fatalf("phase %v, deletes %+v; want a second delete for b", st.phase, deletes)
			}
			requireOnlyAnswered(t, fx, first)
			st, fx = stepAll(t, st, ended(releaseRemoved))
			if st.phase != holderGone {
				t.Fatalf("phase %v, want gone", st.phase)
			}
			requireOnlyAnswered(t, fx, second)
		})
	}
}

func TestAHoldAfterAJoinedReleaseKeepsTheTorrent(t *testing.T) {
	t.Parallel()
	joined := make(chan releaseResult, 1)
	st, fx := stepAll(t, holderState{}, hold("a"), hold("b"),
		release("a", true, nil), release("b", true, joined), hold("b"), ended(releaseHeldElsewhere))
	if st.phase != holderHeld || !slices.Equal(sortedHolds(st.holds), []string{"b"}) {
		t.Fatalf("phase %v holds %v; want held by b", st.phase, sortedHolds(st.holds))
	}
	if n := len(effectsOf(fx, fxHolderDelete)); n != 1 {
		t.Fatalf("%d deletes, want only a's", n)
	}
	requireOnlyAnswered(t, fx, joined)
}

func TestASilentJoinedReleaseSurvivesAFailedAttempt(t *testing.T) {
	t.Parallel()
	st, _ := stepAll(t, holderState{}, hold("a"), hold("b"),
		release("a", true, nil), release("b", true, nil), ended(releaseFailed))
	st, fx := stepAll(t, st, holderEvent{kind: holderRetry}, ended(releaseHeldElsewhere))
	deletes := effectsOf(fx, fxHolderDelete)
	if st.phase != holderDeleting || len(deletes) != 2 || deletes[1].call.releasing != "b" {
		t.Fatalf("phase %v, deletes %+v; want a's retry then b's own delete", st.phase, deletes)
	}
}

func requireOnlyAnswered(t *testing.T, fx []holderEffect, ch chan releaseResult) {
	t.Helper()
	answers := effectsOf(fx, fxHolderAnswer)
	if ch == nil {
		if len(answers) != 0 {
			t.Fatalf("answers %+v, want none", answers)
		}
		return
	}
	if len(answers) != 1 || answers[0].ch != ch {
		t.Fatalf("answers %+v, want exactly one to the expected waiter", answers)
	}
}

func TestAReleaseWhileGoneIsRemoved(t *testing.T) {
	t.Parallel()
	done := make(chan releaseResult, 1)
	_, fx := stepAll(
		t, holderState{}, hold("a"), release("a", true, nil), ended(releaseRemoved), release("b", true, done),
	)
	answers := effectsOf(fx, fxHolderAnswer)
	if len(answers) != 1 || answers[0].result.outcome != releaseRemoved {
		t.Fatalf("answers %+v, want one removed", answers)
	}
}

func TestAHoldAfterGoneIsToldItsPlacementIsGone(t *testing.T) {
	t.Parallel()
	_, fx := stepAll(t, holderState{}, hold("a"), release("a", true, nil), ended(releaseRemoved), hold("b"))
	gone := effectsOf(fx, fxHolderGone)
	if len(gone) != 1 || gone[0].grab != "b" {
		t.Fatalf("gone effects %+v, want one for b", gone)
	}
}

func TestAHoldDuringTheDeleteIsToldWhenItSucceeds(t *testing.T) {
	t.Parallel()
	_, fx := stepAll(t, holderState{}, hold("a"), release("a", true, nil), hold("b"), ended(releaseRemoved))
	gone := effectsOf(fx, fxHolderGone)
	if len(gone) != 1 || gone[0].grab != "b" {
		t.Fatalf("gone effects %+v, want one for b", gone)
	}
}

func TestAHoldDuringAFailedDeleteIsKept(t *testing.T) {
	t.Parallel()
	st, _ := stepAll(t, holderState{}, hold("a"), release("a", true, nil), hold("b"), ended(releaseStopped))
	if st.phase != holderHeld || !slices.Equal(sortedHolds(st.holds), []string{"b"}) {
		t.Fatalf("phase %v holds %v, want held by b", st.phase, st.holds)
	}
}

func TestASuccessfulDeleteOfASlotHoldingPlacementFreesTheSlot(t *testing.T) {
	t.Parallel()
	for _, slot := range []bool{true, false} {
		ev := release("a", true, nil)
		ev.slot = slot
		_, fx := stepAll(t, holderState{}, hold("a"), ev, ended(releaseRemoved))
		if got := len(effectsOf(fx, fxHolderSlot)); (got == 1) != slot {
			t.Fatalf("slot=%v: slot effects %d", slot, got)
		}
	}
}

func TestASilentReleaseRetriesAFailedDeleteThenGivesUp(t *testing.T) {
	t.Parallel()
	st, _ := stepAll(t, holderState{}, hold("a"), release("a", true, nil))
	for i := 1; i <= holderMaxRetries; i++ {
		var fx []holderEffect
		st, fx = stepAll(t, st, ended(releaseFailed))
		if st.phase != holderDeleting || len(effectsOf(fx, fxHolderRetry)) != 1 {
			t.Fatalf("failure %d: phase %v retries %d, want a retry", i, st.phase, len(effectsOf(fx, fxHolderRetry)))
		}
	}
	st, _ = stepAll(t, st, ended(releaseFailed))
	if st.phase != holderIdle {
		t.Fatalf("phase %v after the retries ran out, want idle", st.phase)
	}
}

func TestAWaitingReleaseIsAnsweredAFailureAtOnce(t *testing.T) {
	t.Parallel()
	done := make(chan releaseResult, 1)
	st, fx := stepAll(t, holderState{}, hold("a"), release("a", true, done), ended(releaseFailed))
	if st.phase != holderIdle || len(effectsOf(fx, fxHolderRetry)) != 0 || len(effectsOf(fx, fxHolderAnswer)) != 1 {
		t.Fatalf("phase %v retries %d answers %d; want idle, no retry, one answer",
			st.phase, len(effectsOf(fx, fxHolderRetry)), len(effectsOf(fx, fxHolderAnswer)))
	}
}

type holderWorld struct {
	mu        sync.Mutex
	lib       map[string]map[string]bool
	rec       map[string]map[string]bool
	deleted   map[string]bool
	inFlight  map[string]int
	deletes   map[string]int
	gone      map[string]int
	slots     int
	violation []string
	failNext  int
	takes     time.Duration
	check     bool
	lastCall  map[string]holderCall
}

func newHolderWorld() *holderWorld {
	return &holderWorld{
		lib: map[string]map[string]bool{}, rec: map[string]map[string]bool{}, deleted: map[string]bool{},
		lastCall: map[string]holderCall{}, inFlight: map[string]int{}, deletes: map[string]int{},
		gone: map[string]int{}, takes: time.Second,
	}
}

func (w *holderWorld) set(m map[string]map[string]bool, grab, id string, v bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if m[grab] == nil {
		m[grab] = map[string]bool{}
	}
	m[grab][id] = v
}

func (w *holderWorld) violatef(format string, args ...any) {
	w.violation = append(w.violation, time.Now().Format("15:04:05")+" "+sprintf(format, args...))
}

func (w *holderWorld) holderBy(id, releasing string, library bool) (string, bool) {
	for g, ids := range w.rec {
		if ids[id] && g != releasing {
			return "queue record of " + g, true
		}
	}
	for g, ids := range w.lib {
		if ids[id] && (g != releasing || !library) {
			return "library record of " + g, true
		}
	}
	return "", false
}

func (w *holderWorld) recount(_, id string, call holderCall, holds []string) (holderRecount, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastCall[id] = call
	var rc holderRecount
	for _, h := range holds {
		if w.rec[h][id] {
			rc.held = true
		} else {
			rc.stale = append(rc.stale, h)
		}
	}
	if rc.held {
		return rc, nil
	}
	_, rc.held = w.holderBy(id, call.releasing, call.library)
	return rc, nil
}

func (w *holderWorld) remove(ctx context.Context, provider, id string) error {
	key := holderKey(provider, id)
	w.mu.Lock()
	w.inFlight[key]++
	if who, ok := w.holderBy(id, w.lastCall[id].releasing, w.lastCall[id].library); ok && w.check {
		w.violatef("deleted %s under the %s", id, who)
	}
	if w.inFlight[key] > 1 {
		w.violatef("two deletes in flight for %s", id)
	}
	fail := w.failNext > 0
	if fail {
		w.failNext--
	}
	takes := w.takes
	w.mu.Unlock()
	select {
	case <-time.After(takes):
	case <-ctx.Done():
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight[key]--
	if fail {
		return errors.New("provider failed")
	}
	w.deletes[id]++
	w.deleted[id] = true
	return nil
}

func (w *holderWorld) newHolder(t *testing.T) *holderActor {
	t.Helper()
	g := task.NewRoot(context.WithoutCancel(t.Context()), "holder-test", zerolog.Nop())
	a := newHolderActor(g, holderDeps{
		logger:  zerolog.Nop(),
		recount: w.recount,
		remove:  w.remove,
		slotFreed: func(string) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.slots++
		},
		gone: func(grab, _, id string) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.gone[grab+"/"+id]++
		},
	})
	t.Cleanup(func() { w.stop(t, g) })
	return a
}

func (w *holderWorld) stop(t *testing.T, g *task.Group) {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), testStopTimeout)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func sendHold(t *testing.T, a *holderActor, id, grab string) {
	t.Helper()
	if err := a.runner.Send(holderKey("p", id), hold(grab)); err != nil {
		t.Fatal(err)
	}
}

func sendRelease(t *testing.T, a *holderActor, id string, ev holderEvent) {
	t.Helper()
	if err := a.runner.Send(holderKey("p", id), ev); err != nil {
		t.Fatal(err)
	}
}

func TestHolderBlockerSequenceDeletesOnlyOnTheLastRelease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newHolderWorld()
		a := w.newHolder(t)
		w.set(w.rec, "a", "t1", true)
		w.set(w.rec, "b", "t1", true)
		sendHold(t, a, "t1", "a")
		sendHold(t, a, "t1", "b")

		w.set(w.rec, "a", "t1", false)
		sendRelease(t, a, "t1", release("a", true, nil))
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		if w.deletes["t1"] != 0 {
			t.Fatalf("deleted while b held it")
		}

		w.set(w.rec, "b", "t1", false)
		sendRelease(t, a, "t1", release("b", false, nil))
		synctest.Wait()
		if w.deletes["t1"] != 0 {
			t.Fatalf("a release without delete deleted")
		}

		done := make(chan releaseResult, 1)
		sendRelease(t, a, "t1", release("a", true, done))
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		if res := <-done; res.outcome != releaseRemoved || w.deletes["t1"] != 1 {
			t.Fatalf("outcome %v deletes %d; want removed once every grab released", res.outcome, w.deletes["t1"])
		}
	})
}

func TestHolderNeverDeletesUnderASameKeyLibraryRecord(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newHolderWorld()
		a := w.newHolder(t)
		w.set(w.lib, "a", "t1", true)
		sendHold(t, a, "t1", "a")
		done := make(chan releaseResult, 1)
		sendRelease(t, a, "t1", release("a", true, done))
		synctest.Wait()
		if res := <-done; res.outcome != releaseHeldElsewhere || w.deletes["t1"] != 0 {
			t.Fatalf("outcome %v deletes %d; want held elsewhere and no delete", res.outcome, w.deletes["t1"])
		}

		own := release("a", true, done)
		own.library = true
		sendRelease(t, a, "t1", own)
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		if res := <-done; res.outcome != releaseRemoved {
			t.Fatalf("outcome %v; want removed once the library record is the one being removed", res.outcome)
		}
	})
}

func TestHolderTellsAHoldAfterGone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newHolderWorld()
		a := w.newHolder(t)
		sendHold(t, a, "t1", "a")
		done := make(chan releaseResult, 1)
		sendRelease(t, a, "t1", release("a", true, done))
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		<-done

		sendHold(t, a, "t1", "b")
		synctest.Wait()
		if w.gone["b/t1"] != 1 {
			t.Fatalf("gone calls %v, want one for b", w.gone)
		}
	})
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func (w *holderWorld) isDeleted(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deleted[id]
}

func (w *holderWorld) libListed(grab, id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lib[grab][id]
}

func (w *holderWorld) setFail(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failNext = n
}

func (w *holderWorld) goneCalls(key string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gone[key]
}

func TestAFireAndForgetDeleteThatGivesUpIsLogged(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		w := newHolderWorld()
		w.setFail(100)
		g := task.NewRoot(context.WithoutCancel(t.Context()), "holder-test", zerolog.Nop())
		a := newHolderActor(g, holderDeps{
			logger:     zerolog.New(&logs),
			recount:    w.recount,
			remove:     w.remove,
			retryDelay: func(int) time.Duration { return time.Second },
		})
		sendRelease(t, a, "t1", release("a", true, nil))
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		w.stop(t, g)

		if !strings.Contains(logs.String(), "Failed to delete a provider torrent that was given up on") {
			t.Fatalf("a delete that gave up after its retries was not logged:\n%s", logs.String())
		}
	})
}
