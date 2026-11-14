package grab

import (
	"slices"
	"testing"
	"time"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
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
		{name: "stalled before pending", st: provider(base),
			v:    DeadlineView{PendingBase: base, Pending: PendingProvider, StalledAfter: time.Hour},
			want: EvStalledDeadline, at: base.Add(time.Hour), fail: FailNoEffect},
		{name: "pending before stalled", st: provider(base),
			v:    DeadlineView{PendingBase: base, Pending: PendingConverting, StalledAfter: 10 * time.Hour},
			want: EvPendingDeadline, at: base.Add(2 * time.Hour), fail: FailEntry},
		{name: "link wait", st: in(PhaseAwaitingLinks), v: DeadlineView{DownloadedAt: base},
			want: EvLinkWaitExpired, at: base.Add(6 * time.Hour), fail: FailBadRelease},
		{name: "link wait without a download time", st: in(PhaseAwaitingLinks), none: true},
		{name: "link wait of a legacy record counts from its phase",
			st:   State{Key: "k", Phase: PhaseAwaitingLinks, PhaseSince: base},
			want: EvLinkWaitExpired, at: base.Add(6 * time.Hour), fail: FailBadRelease},
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
		Retention: time.Hour, SeenFailure: at(), SubmitBase: at(), SubmitBudget: time.Minute,
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
		case PhaseQueued, PhaseThrottled, PhaseActing, PhaseDeleting:
			if hasDeadline {
				t.Errorf("%v has a timer or child of its own yet also a derived deadline", p)
			}
		case PhaseSubmitting, PhaseProvider, PhaseAwaitingLinks, PhaseFailed:
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
	first := Event{Kind: EvProviderObserved, Observed: debridTypes.PhaseStalled, At: at()}
	st, fx, err := Transition(st, first)
	if err != nil || !st.StalledSince.Equal(at()) || len(fx) != 1 || fx[0].Kind != FxPersist {
		t.Fatalf("first stalled poll: %+v %v %v", st.StalledSince, fx, err)
	}
	again := Event{Kind: EvProviderObserved, Observed: debridTypes.PhaseStalled, At: at().Add(time.Minute)}
	st, fx, err = Transition(st, again)
	if err != nil || !st.StalledSince.Equal(at()) || len(fx) != 0 {
		t.Fatalf("second stalled poll: %+v %v %v", st.StalledSince, fx, err)
	}
	st, fx, err = Transition(st, Event{Kind: EvProviderObserved, At: at().Add(2 * time.Minute)})
	if err != nil || !st.StalledSince.IsZero() || len(fx) != 1 {
		t.Fatalf("poll that is not stalled: %+v %v %v", st.StalledSince, fx, err)
	}
}

func TestUnknownRunArmsTheStalledDeadline(t *testing.T) {
	t.Parallel()
	v := DeadlineView{StalledAfter: time.Hour}
	st := State{Key: "k", Phase: PhaseProvider, UnknownRaw: "x", StalledSince: at()}
	ev, due, ok := NextDeadline(st, v)
	if !ok || ev.Kind != EvStalledDeadline || !due.Equal(at().Add(time.Hour)) || ev.Fail != FailNoEffect {
		t.Fatalf("unknown run: %+v %v %v", ev, due, ok)
	}
	if want := `provider state "x" unknown for 1h0m0s`; ev.Reason != want {
		t.Fatalf("unknown run reason %q, want %q", ev.Reason, want)
	}
	failed, _, err := Transition(st, ev)
	if err != nil || failed.Failure.Reason != ev.Reason {
		t.Fatalf("unknown run failed as %+v %v", failed.Failure, err)
	}
	st.UnknownRaw = ""
	ev, _, ok = NextDeadline(st, v)
	if !ok || ev.Fail != FailNoEffect {
		t.Fatalf("stall: %+v %v", ev, ok)
	}
	failed, _, err = Transition(st, ev)
	if err != nil || failed.Failure.Reason != "stalled" {
		t.Fatalf("stall failed as %+v %v", failed.Failure, err)
	}
}

func TestSubmittingDeadlines(t *testing.T) {
	t.Parallel()
	base := at()
	budget := 21 * time.Minute
	nzb := in(PhaseSubmitting)
	nzb.NZB = true
	cases := []struct {
		name   string
		st     State
		v      DeadlineView
		want   EventKind
		at     time.Time
		fail   FailEffect
		reason string
		none   bool
	}{
		{name: "submit running", st: in(PhaseSubmitting),
			v:    DeadlineView{SubmitBase: base, SubmitBudget: budget, SubmitRunning: true},
			want: EvSubmitDeadline, at: base.Add(budget)},
		{name: "submit not running", st: in(PhaseSubmitting),
			v:    DeadlineView{SubmitBase: base, SubmitBudget: budget},
			want: EvLookupDeadline, at: base.Add(budget + LookupLimit), fail: FailNoEffect,
			reason: "submit outcome unknown"},
		{name: "nzb", st: nzb, v: DeadlineView{SubmitBase: base, SubmitBudget: budget, SubmitRunning: true},
			none: true},
		{name: "zero base", st: in(PhaseSubmitting), v: DeadlineView{SubmitBudget: budget}, none: true},
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
			if !ok || ev.Kind != c.want || !due.Equal(c.at) || ev.Fail != c.fail || ev.Reason != c.reason {
				t.Fatalf("got %v at %v fail %v reason %q (ok %v), want %v at %v fail %v reason %q",
					ev.Kind, due, ev.Fail, ev.Reason, ok, c.want, c.at, c.fail, c.reason)
			}
		})
	}
}

func TestUnknownCappedWithoutStalledAfter(t *testing.T) {
	t.Parallel()
	st := State{Key: "k", Phase: PhaseProvider, UnknownRaw: "x", StalledSince: at()}
	ev, due, ok := NextDeadline(st, DeadlineView{})
	if !ok || ev.Kind != EvStalledDeadline || !due.Equal(at().Add(PendingLimit)) || ev.Fail != FailNoEffect {
		t.Fatalf("%+v at %v (ok %v), want a stall deadline at %v", ev, due, ok, at().Add(PendingLimit))
	}
	if want := `provider state "x" unknown for 6h0m0s`; ev.Reason != want {
		t.Fatalf("capped unknown run reason %q, want %q", ev.Reason, want)
	}
	v := DeadlineView{PendingBase: at(), Pending: PendingConverting}
	if ev, due, _ = NextDeadline(st, v); ev.Kind != EvPendingDeadline || !due.Equal(at().Add(ConversionLimit)) {
		t.Fatalf("an earlier pending deadline lost to the cap: %v at %v", ev.Kind, due)
	}
	st.UnknownRaw = ""
	if ev, due, ok = NextDeadline(st, DeadlineView{}); ok {
		t.Fatalf("a plain stall without a limit got %v at %v, want none", ev.Kind, due)
	}
}
