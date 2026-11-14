package grab

import (
	"cmp"
	"fmt"
	"time"

	"github.com/sirrobot01/decypharr/internal/state"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// reasonBusy is why a Submit is refused while the grab is being deleted.
const reasonBusy = "busy"

// notAccepted is why an event the phase has no row for is refused.
const notAccepted = "not accepted"

// Transition is the grab's one transition function. It is pure: it returns the
// next state and the effects the actor must apply, in order. A phase change
// sets PhaseSince to the event's time and persists first. On a refusal it
// returns st unchanged, no effects and a *RefusedError.
func Transition(st State, ev Event) (State, []Effect, error) {
	next, fx, err := step(st, ev)
	if err != nil {
		return st, nil, err
	}
	return next, fx, nil
}

func step(st State, ev Event) (State, []Effect, error) {
	if st.Phase == PhaseAbsent {
		return absent(st, ev)
	}
	if st.Phase == PhaseDeleting {
		return deleting(st, ev)
	}
	switch {
	case ev.Kind.is(EvTagsChanged, EvCategoryChanged, EvRemoveFile):
		return st, []Effect{{Kind: FxPersist}}, nil
	case ev.Kind.is(EvDelete):
		return remove(st, ev, ev.Cleanup)
	case ev.Kind.is(EvSubmit):
		return submit(st, ev)
	case ev.Kind.is(EvRecover):
		return resume(st, ev)
	case ev.Kind.is(EvProviderObserved):
		return observe(st, ev)
	}
	return byPhase(st, ev)
}

// byPhase applies the rows of the phase the grab is in.
func byPhase(st State, ev Event) (State, []Effect, error) {
	switch st.Phase {
	case PhaseQueued:
		return queued(st, ev)
	case PhaseThrottled:
		if ev.Kind == EvSlotFreed || ev.Kind == EvThrottleTimer {
			return enter(st, PhaseQueued, ev, Effect{Kind: FxRequestAdmission})
		}
	case PhaseSubmitting:
		return submitting(st, ev)
	case PhaseProvider:
		return provider(st, ev)
	case PhaseAwaitingLinks:
		return awaitingLinks(st, ev)
	case PhaseActing:
		return acting(st, ev)
	case PhaseComplete:
		if ev.Kind == EvProviderDropped {
			return fail(st, ev, ev.Fail, "dropped by the provider")
		}
		if ev.Kind == EvActionNone {
			return remove(st, ev, Cleanup{})
		}
	case PhaseFailed:
		if ev.Kind == EvFailedRetention {
			return remove(st, ev, Cleanup{})
		}
	case PhaseAbsent, PhaseDeleting: // handled by step
	}
	return refuse(st, ev, notAccepted)
}

func absent(st State, ev Event) (State, []Effect, error) {
	if ev.Kind != EvSubmit {
		return refuse(st, ev, "no record")
	}
	st.PackKey = ev.PackKey
	if st.season() {
		return enter(st, PhaseActing, ev, startChild(ChildAction))
	}
	return enter(st, PhaseQueued, ev, Effect{Kind: FxRequestAdmission})
}

// deleting ignores every event it does not name: late child results arrive after a Delete.
func deleting(st State, ev Event) (State, []Effect, error) {
	switch {
	case ev.Kind.is(EvSubmit, EvAdmitted):
		return refuse(st, ev, reasonBusy)
	case ev.Kind.is(EvSubmitResult, EvParseResult, EvLookup):
		if ev.Outcome == OutcomePlacement || ev.Outcome == OutcomeFound {
			return st, []Effect{{Kind: FxReleasePlacement, Provider: ev.Provider, TorrentID: ev.TorrentID}}, nil
		}
	case ev.Kind.is(EvCleanupDone):
		next, fx, err := enter(st, PhaseAbsent, ev, Effect{Kind: FxRemoveRecord})
		return State{Key: st.Key, Phase: next.Phase, PhaseSince: next.PhaseSince}, fx, err
	case ev.Kind.is(EvRecover):
		return st, []Effect{cleanupChild(st.Cleanup)}, nil
	}
	return st, nil, nil
}

// remove starts the cleanup path; the actor waits for every cancelled child to exit before the cleanup child runs.
func remove(st State, ev Event, c Cleanup) (State, []Effect, error) {
	st.Cleanup = c
	var fx []Effect
	if st.Phase == PhaseQueued || st.Phase == PhaseThrottled {
		// In Submitting the in-flight call holds the slot; the submit child's exit frees it.
		fx = append(fx, Effect{Kind: FxCancelAdmission})
	}
	fx = append(fx, Effect{Kind: FxCancelChildren}, cleanupChild(c))
	return enter(st, PhaseDeleting, ev, fx...)
}

func submit(st State, ev Event) (State, []Effect, error) {
	if st.Phase != PhaseFailed {
		return st, nil, nil // a re-add of a grab already in hand
	}
	if ev.PackKey != "" {
		st.PackKey = ev.PackKey
		return enter(st, PhaseActing, ev, startChild(ChildAction))
	}
	if st.season() {
		return refuse(st, ev, "season child")
	}
	if st.NoSubmit != "" {
		return refuse(st, ev, st.NoSubmit)
	}
	return enter(st, PhaseQueued, ev, Effect{Kind: FxRequestAdmission})
}

func resume(st State, ev Event) (State, []Effect, error) {
	switch st.Phase {
	case PhaseQueued:
		return st, []Effect{{Kind: FxRequestAdmission}}, nil
	case PhaseThrottled:
		return enter(st, PhaseQueued, ev, Effect{Kind: FxRequestAdmission})
	case PhaseSubmitting:
		if st.NZB {
			// An NZB submit is local: nothing can have reached a provider.
			return enter(st, PhaseQueued, ev, Effect{Kind: FxRequestAdmission})
		}
		return lookupAfterSubmit(st)
	case PhaseProvider:
		return st, []Effect{startChild(ChildPoll)}, nil
	case PhaseAwaitingLinks:
		return st, []Effect{startChild(ChildLinks)}, nil
	case PhaseActing:
		return st, []Effect{startChild(ChildAction)}, nil
	case PhaseAbsent, PhaseComplete, PhaseFailed, PhaseDeleting:
	}
	return st, nil, nil
}

// observe handles a provider observation. Only Provider and AwaitingLinks
// take one: in Acting a second Downloaded must never start a second action.
func observe(st State, ev Event) (State, []Effect, error) {
	observed := observedPhase(ev)
	switch st.Phase {
	case PhaseProvider:
	case PhaseAwaitingLinks:
		if observed == debridTypes.PhaseFailed {
			return fail(st, ev, ev.Fail, "failed at the provider")
		}
		return refuse(st, ev, notAccepted) // the links child decides when it is ready
	case PhaseAbsent, PhaseQueued, PhaseThrottled, PhaseSubmitting, PhaseActing, PhaseComplete, PhaseFailed,
		PhaseDeleting:
		return refuse(st, ev, "contradicts "+st.Phase.String())
	}
	if observed == debridTypes.PhaseFailed {
		return fail(st, ev, ev.Fail, "failed at the provider")
	}
	if observed == debridTypes.PhaseUnknown {
		next := st
		next.UnknownRaw, next.StalledSince = ev.Raw, cmp.Or(st.StalledSince, ev.At)
		return persistIfChanged(st, next)
	}
	if st.ProviderPhase != debridTypes.PhaseNone && observed != st.ProviderPhase &&
		!debridTypes.LegalEdge(st.ProviderPhase, observed) {
		return refuse(st, ev, fmt.Sprintf("provider phase %v -> %v", st.ProviderPhase, observed))
	}
	if observed == debridTypes.PhaseDownloaded {
		ev.Downloaded = true
		return placed(st, ev)
	}
	next := st
	next.ProviderPhase, next.UnknownRaw = observed, ""
	switch {
	case observed != debridTypes.PhaseStalled:
		next.StalledSince = time.Time{}
	case st.UnknownRaw != "":
		next.StalledSince = ev.At
	default:
		next.StalledSince = cmp.Or(st.StalledSince, ev.At)
	}
	return persistIfChanged(st, next)
}

func observedPhase(ev Event) debridTypes.Phase {
	switch {
	case ev.Failed:
		return debridTypes.PhaseFailed
	case ev.Observed != debridTypes.PhaseNone:
		return ev.Observed
	case ev.Downloaded:
		return debridTypes.PhaseDownloaded
	}
	return debridTypes.PhaseDownloading
}

func persistIfChanged(st, next State) (State, []Effect, error) {
	if next == st {
		return st, nil, nil
	}
	return next, []Effect{{Kind: FxPersist}}, nil
}

func queued(st State, ev Event) (State, []Effect, error) {
	switch {
	case ev.Kind.is(EvAdmissionDeadline):
		return st, []Effect{{Kind: FxRequestAdmission}}, nil
	case ev.Kind.is(EvAdmitted):
		st.SubmitAttempt = SubmitAttempt{At: ev.At}
		return enter(st, PhaseSubmitting, ev, startChild(ChildSubmit))
	}
	return refuse(st, ev, notAccepted)
}

func submitting(st State, ev Event) (State, []Effect, error) {
	switch {
	case ev.Kind.is(EvSubmitResult, EvParseResult):
		return submitResult(st, ev)
	case ev.Kind.is(EvSubmitDeadline):
		return lookupAfterSubmit(st)
	case ev.Kind.is(EvLookup):
		return lookupResult(st, ev)
	case ev.Kind.is(EvLookupDeadline):
		return fail(st, ev, FailNoEffect, "submit outcome unknown")
	}
	return refuse(st, ev, notAccepted)
}

func submitResult(st State, ev Event) (State, []Effect, error) {
	if ev.Provider != "" {
		st.SubmitAttempt.Provider = ev.Provider
	}
	switch {
	case ev.Outcome.is(OutcomeThrottled):
		return enter(st, PhaseThrottled, ev)
	case ev.Outcome.is(OutcomePlacement):
		return placed(st, ev)
	case ev.Outcome.is(OutcomePermanent):
		return fail(st, ev, ev.Fail, "refused by the provider")
	case ev.Outcome.is(OutcomeUnknown) && ev.Kind == EvSubmitResult:
		return lookupAfterSubmit(st)
	}
	return refuse(st, ev, notAccepted)
}

func lookupResult(st State, ev Event) (State, []Effect, error) {
	switch {
	case ev.Outcome.is(OutcomeFound):
		return placed(st, ev)
	case ev.Outcome.is(OutcomeAbsent):
		return enter(st, PhaseQueued, ev, Effect{Kind: FxRequestAdmission})
	}
	return refuse(st, ev, notAccepted)
}

func lookupAfterSubmit(st State) (State, []Effect, error) {
	return st, []Effect{{Kind: FxStartChild, Child: ChildLookup, AfterExit: ChildSubmit, HasAfter: true}}, nil
}

func provider(st State, ev Event) (State, []Effect, error) {
	switch {
	case ev.Kind.is(EvPollTimer):
		return st, nil, nil
	case ev.Kind.is(EvPendingDeadline):
		return fail(st, ev, ev.Fail, "pending too long")
	case ev.Kind.is(EvPlacementGone):
		return fail(st, ev, ev.Fail, "gone from the provider")
	case ev.Kind.is(EvStalledDeadline):
		// A stall is the provider's, not the release's: never tell the Arr the release is bad.
		return fail(st, ev, FailNoEffect, "stalled")
	}
	return refuse(st, ev, notAccepted)
}

func awaitingLinks(st State, ev Event) (State, []Effect, error) {
	switch {
	case ev.Kind.is(EvLinksReady):
		return enter(st, PhaseActing, ev, startChild(ChildAction))
	case ev.Kind.is(EvLinkWaitExpired):
		return fail(st, ev, ev.Fail, "links never ready")
	case ev.Kind.is(EvPlacementGone):
		return fail(st, ev, ev.Fail, "gone from the provider")
	}
	return refuse(st, ev, notAccepted)
}

func acting(st State, ev Event) (State, []Effect, error) {
	switch {
	case ev.Kind.is(EvActionProgress, EvChildDone, EvSeasonsStarted):
		return st, nil, nil
	case ev.Kind.is(EvActionDone):
		return enter(st, PhaseComplete, ev)
	case ev.Kind.is(EvActionFailed):
		return fail(st, ev, ev.Fail, "action failed")
	}
	return refuse(st, ev, notAccepted)
}

// placed moves a grab the provider has to the phase its download state calls for.
func placed(st State, ev Event) (State, []Effect, error) {
	switch {
	case !ev.Downloaded:
		return enter(st, PhaseProvider, ev, startChild(ChildPoll))
	case !ev.AllLinked:
		return enter(st, PhaseAwaitingLinks, ev, startChild(ChildLinks))
	}
	return enter(st, PhaseActing, ev, startChild(ChildAction))
}

func fail(st State, ev Event, effect FailEffect, reason string) (State, []Effect, error) {
	st.Failure = Failure{Reason: cmp.Or(ev.Reason, reason), At: ev.At, Count: st.Failure.Count + 1}
	return enter(st, PhaseFailed, ev, Effect{Kind: FxFail, Fail: effect})
}

// enter moves st to another phase: the table must list the move, and the record is persisted first.
func enter(st State, to Phase, ev Event, fx ...Effect) (State, []Effect, error) {
	if !state.Legal(successors, st.Phase, to) {
		return refuse(st, ev, "no move to "+to.String())
	}
	st.Phase, st.PhaseSince, st.StalledSince = to, ev.At, time.Time{}
	st.ProviderPhase, st.UnknownRaw = debridTypes.PhaseNone, ""
	return st, append([]Effect{{Kind: FxPersist}}, fx...), nil
}

func refuse(st State, ev Event, why string) (State, []Effect, error) {
	return st, nil, &RefusedError{Phase: st.Phase, Event: ev.Kind, Why: why}
}

func startChild(c ChildKind) Effect { return Effect{Kind: FxStartChild, Child: c} }

func cleanupChild(c Cleanup) Effect {
	return Effect{Kind: FxStartChild, Child: ChildCleanup, Cleanup: c}
}
