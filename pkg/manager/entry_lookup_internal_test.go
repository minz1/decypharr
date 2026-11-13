package manager

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

// answerLookups answers every lookup of grab a with no verdict until the grab leaves Submitting or the clock reaches
// until, and returns how many ran.
func (h *actorHarness) answerLookups(until time.Time) int {
	h.t.Helper()
	n := 0
	for time.Now().Before(until) && h.phase("a") == grab.PhaseSubmitting {
		select {
		case run := <-h.started:
			if run.call.kind != grab.ChildLookup {
				h.t.Fatalf("started %s, want a lookup", childKind(run.call.kind))
			}
			n++
			run.reply <- entryMsg{}
		case <-time.After(min(time.Minute, time.Until(until))):
		}
		synctest.Wait()
	}
	return n
}

// failedAt is when key's grab failed with reason, as its record says.
func (h *actorHarness) failedAt(key, reason string) time.Time {
	h.t.Helper()
	g, ok := h.diskGrab(key)
	if !ok || g.State.Phase != grab.PhaseFailed || g.State.Failure.Reason != reason {
		h.t.Fatalf("record %+v (found %v), want failed with %q", g.State, ok, reason)
	}
	if fails := h.failsSeen(); !slices.Equal(fails, []grab.FailEffect{grab.FailNoEffect}) {
		h.t.Fatalf("fail effects %v, want one with no effect", fails)
	}
	return g.State.Failure.At
}

func seedSubmitting(t *testing.T, h *actorHarness, at time.Time) {
	t.Helper()
	seed(t, h.store, testEntry("a"), grab.State{Phase: grab.PhaseSubmitting, SubmitAttempt: grab.SubmitAttempt{At: at}})
}

func TestRestartedSubmittingFailsAtLookupDeadline(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		at := time.Now()
		seedSubmitting(t, h, at)
		recoverAll(h)
		due := at.Add(h.a.submitBudget + grab.LookupLimit)

		before := h.answerLookups(due.Add(-time.Second))
		h.mustBe("a", grab.PhaseSubmitting)
		after := h.answerLookups(due.Add(2 * time.Minute))
		if got := h.failedAt("a", "submit outcome unknown"); !got.Equal(due) {
			t.Fatalf("failed at %v, want %v", got, due)
		}
		if n := before + after; n < 2 || n >= int(grab.LookupLimit/tooManyDownloadsRetryDelay) {
			t.Fatalf("%d lookups ran, want a backed-off retry", n)
		}
	})
}

func TestRestartAfterDowntimeStillLooksUp(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		seedSubmitting(t, h, time.Now().Add(-3*time.Hour))
		loaded := time.Now()
		recoverAll(h)

		n := h.answerLookups(loaded.Add(grab.LookupLimit + 2*time.Minute))
		if got := h.failedAt("a", "submit outcome unknown"); got.Before(loaded.Add(grab.LookupLimit)) {
			t.Fatalf("failed at %v, before a full lookup window from the load at %v", got, loaded)
		}
		if n < 2 {
			t.Fatalf("%d lookups ran, want at least 2", n)
		}
	})
}

func TestZeroSubmitAttemptAtDoesNotFireOnLoad(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		seedSubmitting(t, h, time.Time{})
		loaded := time.Now()
		recoverAll(h)
		due := loaded.Add(h.a.submitBudget + grab.LookupLimit)

		h.answerLookups(due.Add(-time.Second))
		h.mustBe("a", grab.PhaseSubmitting)
		h.answerLookups(due.Add(2 * time.Minute))
		if got := h.failedAt("a", "submit outcome unknown"); !got.Equal(due) {
			t.Fatalf("failed at %v, want %v", got, due)
		}
	})
}

func TestSubmitDeadlineStartsLookupWhileSubmitHangs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.submitting("a")

		time.Sleep(h.a.submitBudget + time.Second)
		h.next(grab.ChildLookup)
		h.none()
		time.Sleep(30 * time.Minute)
		h.none()
		lookups := 0
		for _, s := range h.logged() {
			if s == "start:"+childKind(grab.ChildLookup).String() {
				lookups++
			}
		}
		if lookups != 1 {
			t.Fatalf("%d lookups started, want 1", lookups)
		}
		h.mustBe("a", grab.PhaseSubmitting)
	})
}
