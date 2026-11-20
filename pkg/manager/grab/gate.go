package grab

import "time"

type LinksVouch struct {
	Fresh     bool
	HeldSince time.Time
}

type Gated struct {
	Event           Event
	Due             time.Time
	WaitingForLinks bool
}

func GatedDeadline(st State, v DeadlineView, l LinksVouch) (Gated, bool) {
	ev, due, ok := NextDeadline(st, v)
	if !ok {
		return Gated{}, false
	}
	g := Gated{Event: ev, Due: due}
	if ev.Kind == EvLinkWaitExpired && !l.Fresh {
		g.Due = laterOf(due, l.HeldSince.Add(LinksLimit))
		g.WaitingForLinks = true
	}
	return g, true
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
