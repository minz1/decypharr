package grab

import (
	"slices"
	"testing"
	"time"
)

func TestNextDeadline(t *testing.T) {
	t.Parallel()
	base := at()
	seen := base.Add(1500 * time.Millisecond)
	provider := func(stalledSince time.Time) State {
		return State{Key: "k", Phase: PhaseProvider, StalledSince: stalledSince}
	}
	failed := func(nzb bool) State {
		st := failedState()
		st.NZB = nzb
		return st
	}
	cases := []struct {
		name string
		st   State
		v    DeadlineView
		want EventKind
		at   time.Time
		fail FailEffect
		none bool
	}{
		{name: "pending provider", st: provider(time.Time{}),
			v:    DeadlineView{PendingBase: base, Pending: PendingProvider},
			want: EvPendingDeadline, at: base.Add(6 * time.Hour), fail: FailEntry},
		{name: "pending provider of a dead release", st: provider(time.Time{}),
			v:    DeadlineView{PendingBase: base, Pending: PendingProvider, Dead: true},
			want: EvPendingDeadline, at: base.Add(6 * time.Hour), fail: FailBadRelease},
		{name: "pending converting", st: provider(time.Time{}),
			v:    DeadlineView{PendingBase: base, Pending: PendingConverting, Dead: true},
			want: EvPendingDeadline, at: base.Add(2 * time.Hour), fail: FailBadRelease},
		{name: "pending without a base", st: provider(time.Time{}),
			v: DeadlineView{Pending: PendingProvider}, none: true},
		{name: "not pending", st: provider(time.Time{}), v: DeadlineView{PendingBase: base}, none: true},
		{name: "stalled", st: provider(base), v: DeadlineView{StalledAfter: time.Hour},
			want: EvStalledDeadline, at: base.Add(time.Hour), fail: FailNoEffect},
		{name: "stalled without a limit", st: provider(base), none: true},
		{name: "stalled before pending", st: provider(base),
			v:    DeadlineView{PendingBase: base, Pending: PendingProvider, StalledAfter: time.Hour},
			want: EvStalledDeadline, at: base.Add(time.Hour), fail: FailNoEffect},
		{name: "pending before stalled", st: provider(base),
			v:    DeadlineView{PendingBase: base, Pending: PendingConverting, StalledAfter: 10 * time.Hour},
			want: EvPendingDeadline, at: base.Add(2 * time.Hour), fail: FailEntry},
		{name: "link wait", st: in(PhaseAwaitingLinks), v: DeadlineView{DownloadedAt: base},
			want: EvLinkWaitExpired, at: base.Add(6 * time.Hour), fail: FailBadRelease},
		{name: "link wait without a download time", st: in(PhaseAwaitingLinks), none: true},
		{name: "retention", st: failed(false), v: DeadlineView{Retention: time.Hour, SeenFailure: base},
			want: EvFailedRetention, at: base.Add(time.Hour)},
		{name: "retention is seen at the second", st: failed(false),
			v:    DeadlineView{Retention: time.Hour, SeenFailure: base.Add(900 * time.Millisecond)},
			want: EvFailedRetention, at: base.Add(time.Hour)},
		{name: "an NZB keeps 24h", st: failed(true), v: DeadlineView{Retention: time.Hour, SeenFailure: base},
			want: EvFailedRetention, at: base.Add(24 * time.Hour)},
		{name: "retention unset", st: failed(false), v: DeadlineView{SeenFailure: base}, none: true},
		{name: "retention unseen", st: failed(false), v: DeadlineView{Retention: time.Hour}, none: true},
		{name: "retention of another failure", st: failed(false),
			v: DeadlineView{Retention: time.Hour, SeenFailure: seen.Add(time.Second)}, none: true},
		{name: "retention while the pack runs", st: failed(false),
			v: DeadlineView{Retention: time.Hour, SeenFailure: base, PackRunning: true}, none: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ev, due, ok := NextDeadline(c.st, c.v)
			if c.none {
				if ok {
					t.Fatalf("deadline %v at %v, want none", ev.Kind, due)
				}
				return
			}
			if !ok || ev.Kind != c.want || !due.Equal(c.at) || ev.Fail != c.fail {
				t.Fatalf("got %v at %v fail %v (ok %v), want %v at %v fail %v",
					ev.Kind, due, ev.Fail, ok, c.want, c.at, c.fail)
			}
		})
	}
}

func TestEveryNonTerminalPhaseHasADeadlineOrAChildInFlight(t *testing.T) {
	t.Parallel()
	maximal := DeadlineView{
		PendingBase: at(), Pending: PendingProvider, DownloadedAt: at(), StalledAfter: time.Hour,
		Retention: time.Hour, SeenFailure: at(),
	}
	for _, p := range AllPhases() {
		st := in(p)
		st.StalledSince = at()
		if p == PhaseFailed {
			st = failedState()
		}
		_, _, hasDeadline := NextDeadline(st, maximal)
		switch p {
		case PhaseAbsent, PhaseComplete:
			if hasDeadline {
				t.Errorf("%v is idle yet has a deadline", p)
			}
		case PhaseQueued, PhaseThrottled, PhaseSubmitting, PhaseActing, PhaseDeleting:
			if hasDeadline {
				t.Errorf("%v has a timer or child of its own yet also a derived deadline", p)
			}
		case PhaseProvider, PhaseAwaitingLinks, PhaseFailed:
			if !hasDeadline {
				t.Errorf("%v waits on the clock yet has no deadline", p)
			}
		}
	}
	if !slices.Contains(AllPhases(), PhaseFailed) {
		t.Fatal("no failed phase")
	}
}

func TestAStalledPollSetsAndClearsStalledSince(t *testing.T) {
	t.Parallel()
	st := in(PhaseProvider)
	first := Event{Kind: EvProviderObserved, Stalled: true, At: at()}
	st, fx, err := Transition(st, first)
	if err != nil || !st.StalledSince.Equal(at()) || len(fx) != 1 || fx[0].Kind != FxPersist {
		t.Fatalf("first stalled poll: %+v %v %v", st.StalledSince, fx, err)
	}
	again := Event{Kind: EvProviderObserved, Stalled: true, At: at().Add(time.Minute)}
	st, fx, err = Transition(st, again)
	if err != nil || !st.StalledSince.Equal(at()) || len(fx) != 0 {
		t.Fatalf("second stalled poll: %+v %v %v", st.StalledSince, fx, err)
	}
	st, fx, err = Transition(st, Event{Kind: EvProviderObserved, At: at().Add(2 * time.Minute)})
	if err != nil || !st.StalledSince.IsZero() || len(fx) != 1 {
		t.Fatalf("poll that is not stalled: %+v %v %v", st.StalledSince, fx, err)
	}
}
