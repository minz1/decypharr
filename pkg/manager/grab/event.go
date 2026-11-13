package grab

import (
	"errors"
	"fmt"
	"slices"
	"time"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// EventKind is what happened to a grab.
type EventKind uint8

const (
	EvSubmit EventKind = iota
	EvAdmissionDeadline
	EvAdmitted
	EvSubmitResult
	EvParseResult
	EvSlotFreed
	EvThrottleTimer
	EvSubmitDeadline
	EvLookup
	EvLookupDeadline
	EvProviderObserved
	EvPollTimer
	EvStalledDeadline
	EvPendingDeadline
	EvPlacementGone
	EvLinksReady
	EvLinkWaitExpired
	EvActionProgress
	EvChildDone
	EvActionDone
	EvActionFailed
	EvProviderDropped
	EvDelete
	EvCleanupDone
	EvFailedRetention
	EvRecover
	EvTagsChanged
	EvCategoryChanged
	EvRemoveFile
	EvActionNone
	EvSeasonsStarted
	evCount // the number of kinds, for tests
)

func (k EventKind) String() string {
	names := [...]string{
		"submit", "admission_deadline", "admitted", "submit_result", "parse_result", "slot_freed",
		"throttle_timer", "submit_deadline", "lookup", "lookup_deadline", "provider_observed", "poll_timer",
		"stalled_deadline", "pending_deadline", "placement_gone", "links_ready", "link_wait_expired",
		"action_progress", "child_done", "action_done", "action_failed", "provider_dropped", "delete",
		"cleanup_done", "failed_retention", "recover", "tags_changed", "category_changed", "remove_file",
		"action_none", "seasons_started",
	}
	if int(k) < len(names) {
		return names[k]
	}
	return fmt.Sprintf("event(%d)", uint8(k))
}

// is reports whether k is one of kinds; a reducer row names the few kinds it takes.
func (k EventKind) is(kinds ...EventKind) bool { return slices.Contains(kinds, k) }

// Outcome is the result a submit, parse or lookup child reports.
type Outcome uint8

const (
	OutcomeNone Outcome = iota
	// OutcomeThrottled means the provider refused for too many active downloads.
	OutcomeThrottled
	// OutcomePlacement means the provider accepted the grab; Provider and TorrentID name it.
	OutcomePlacement
	// OutcomeUnknown means the call's result is not known (timeout, connection reset).
	OutcomeUnknown
	// OutcomePermanent means the provider refused the grab for good.
	OutcomePermanent
	// OutcomeFound means a lookup found the grab at the provider.
	OutcomeFound
	// OutcomeAbsent means a lookup saw two complete listings, a settle delay apart, without the grab.
	OutcomeAbsent
	outcomeCount // the number of outcomes, for tests
)

func (o Outcome) is(outcomes ...Outcome) bool { return slices.Contains(outcomes, o) }

// FailEffect is what failing a grab does outside the record.
type FailEffect uint8

const (
	// FailNoEffect only marks the record failed.
	FailNoEffect FailEffect = iota
	// FailEntry fails the entry the way today's processor does.
	FailEntry
	// FailBadRelease also tells the Arr the release is bad, so it searches for another.
	FailBadRelease
)

// Event is one input to Transition. Only the fields its Kind uses are set.
type Event struct {
	Kind EventKind
	At   time.Time
	// PackKey on a Submit makes the grab a season child of that pack.
	PackKey string
	// Outcome of a SubmitResult, ParseResult or Lookup.
	Outcome Outcome
	// Downloaded and AllLinked describe a placement result or observation; Failed is a provider Failed observation.
	Downloaded, AllLinked, Failed bool
	Observed                      debridTypes.Phase
	// Raw is an unrecognised provider status, shown as is.
	Raw    string
	Reason string
	// Seasons are the season numbers a pack fanned out into.
	Seasons []int
	Fail    FailEffect
	// Cleanup is what a Delete cleans up.
	Cleanup Cleanup
	// Provider and TorrentID name the placement a SubmitResult or Lookup carries.
	Provider, TorrentID string
}

// EffectKind is what the actor must do.
type EffectKind uint8

const (
	FxPersist EffectKind = iota
	FxRequestAdmission
	FxCancelAdmission
	FxStartChild
	FxCancelChildren
	FxReleasePlacement
	FxFail
	FxRemoveRecord
)

// ChildKind is a child task the actor runs for a grab.
type ChildKind uint8

const (
	ChildSubmit ChildKind = iota
	ChildLookup
	ChildPoll
	ChildLinks
	ChildAction
	ChildCleanup
)

// Effect is one thing Transition asks the actor to do, in order.
type Effect struct {
	Kind EffectKind
	// Child is the child a StartChild starts.
	Child ChildKind
	// AfterExit, when HasAfter, makes the child wait for the running child of that kind to exit.
	AfterExit ChildKind
	HasAfter  bool
	Fail      FailEffect
	// Cleanup is what a StartChild(Cleanup) cleans up.
	Cleanup Cleanup
	// Provider and TorrentID name the placement a ReleasePlacement releases.
	Provider, TorrentID string
}

// RefusedError is Transition refusing an event in a phase.
type RefusedError struct {
	Phase Phase
	Event EventKind
	Why   string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("grab: %v refused in %v: %s", e.Event, e.Phase, e.Why)
}

// IsRefused reports whether err is a refusal by Transition.
func IsRefused(err error) bool {
	var r *RefusedError
	return errors.As(err, &r)
}
