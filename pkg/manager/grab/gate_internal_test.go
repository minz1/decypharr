package grab

import (
	"testing"
	"time"
)

func TestGatedDeadline(t *testing.T) {
	t.Parallel()
	now := at()
	links := in(PhaseAwaitingLinks)
	cases := []struct {
		name    string
		st      State
		v       DeadlineView
		l       LinksVouch
		ok      bool
		due     time.Time
		waiting bool
	}{
		{name: "restored links wait is held", st: links, v: DeadlineView{DownloadedAt: now.Add(-7 * time.Hour)},
			l: LinksVouch{HeldSince: now}, ok: true, due: now.Add(6 * time.Hour), waiting: true},
		{name: "fresh links wait is HEAD's", st: links, v: DeadlineView{DownloadedAt: now.Add(-7 * time.Hour)},
			l: LinksVouch{Fresh: true, HeldSince: now}, ok: true, due: now.Add(-time.Hour)},
		{name: "no download time, not fresh", st: links, l: LinksVouch{HeldSince: now}},
		{name: "no download time, fresh", st: links, l: LinksVouch{Fresh: true, HeldSince: now}},
		{name: "held before the download finished", st: links, v: DeadlineView{DownloadedAt: now},
			l: LinksVouch{HeldSince: now.Add(-time.Hour)}, ok: true, due: now.Add(6 * time.Hour), waiting: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			g, ok := GatedDeadline(c.st, c.v, c.l)
			if ok != c.ok {
				t.Fatalf("ok %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if g.Event.Kind != EvLinkWaitExpired || g.Event.Fail != FailBadRelease || !g.Due.Equal(c.due) ||
				g.WaitingForLinks != c.waiting {
				t.Fatalf("got %v %v due %v waiting %v, want due %v waiting %v",
					g.Event.Kind, g.Event.Fail, g.Due, g.WaitingForLinks, c.due, c.waiting)
			}
		})
	}
}

type gateCase struct {
	st        State
	v         DeadlineView
	fresh     bool
	heldSince time.Time
	phase     Phase
}

func gateStates(p Phase) []State {
	sts := []State{in(p)}
	if p == PhaseProvider {
		sts = append(sts, State{Key: "k", Phase: PhaseProvider, StalledSince: at()})
	}
	if p == PhaseFailed {
		sts = append(sts, failedState())
	}
	return sts
}

func gateViews() []DeadlineView {
	now := at()
	return []DeadlineView{
		{},
		{PendingBase: now, Pending: PendingProvider},
		{PendingBase: now, Pending: PendingConverting, Dead: true},
		{StalledAfter: time.Hour},
		{Retention: time.Hour, SeenFailure: failedState().Failure.At},
		{SubmitBase: now, SubmitBudget: time.Minute, SubmitRunning: true},
		{SubmitBase: now, SubmitBudget: time.Minute},
	}
}

func gateCases() []gateCase {
	now := at()
	times := []time.Time{{}, now.Add(-7 * time.Hour), now.Add(-time.Hour), now}
	var out []gateCase
	for _, p := range AllPhases() {
		for _, st := range gateStates(p) {
			for _, v := range gateViews() {
				for _, downloaded := range times {
					v.DownloadedAt = downloaded
					out = append(out, gateTimes(st, v, p, times)...)
				}
			}
		}
	}
	return out
}

func gateTimes(st State, v DeadlineView, p Phase, times []time.Time) []gateCase {
	var out []gateCase
	for _, held := range times {
		for _, fresh := range []bool{false, true} {
			out = append(out, gateCase{st: st, v: v, fresh: fresh, heldSince: held, phase: p})
		}
	}
	return out
}

func TestGateKeepsHEADsVerdict(t *testing.T) {
	t.Parallel()
	for _, c := range gateCases() {
		head, headDue, headOK := NextDeadline(c.st, c.v)
		g, ok := GatedDeadline(c.st, c.v, LinksVouch{Fresh: c.fresh, HeldSince: c.heldSince})
		if ok != headOK {
			t.Fatalf("%v: ok %v, HEAD %v", c.phase, ok, headOK)
		}
		if !ok {
			continue
		}
		if g.Event.Kind != head.Kind || g.Event.Fail != head.Fail || g.Event.Reason != head.Reason {
			t.Fatalf("%v: event %+v, HEAD %+v", c.phase, g.Event, head)
		}
		if g.Due.Before(headDue) {
			t.Fatalf("%v: due %v before HEAD's %v", c.phase, g.Due, headDue)
		}
		held := !c.fresh && head.Kind == EvLinkWaitExpired
		if !held && (!g.Due.Equal(headDue) || g.WaitingForLinks) {
			t.Fatalf("%v kind %v fresh %v: due %v waiting %v, HEAD due %v",
				c.phase, head.Kind, c.fresh, g.Due, g.WaitingForLinks, headDue)
		}
		if held && !g.WaitingForLinks {
			t.Fatalf("%v: not waiting without a fresh mark", c.phase)
		}
	}
}
