// Package grab is the state machine of one grab: a download decypharr was asked
// to fetch, from the request until the finished files are in the library or the
// grab has failed. It is pure: a typed Phase, a successors table and one
// transition function that returns the effects for its owner to apply.
package grab

import "fmt"

// Phase is where a grab is in its life.
type Phase uint8

const (
	// PhaseAbsent means there is no record for the key.
	PhaseAbsent Phase = iota
	// PhaseQueued waits for an admission slot at its provider.
	PhaseQueued
	// PhaseThrottled was refused by the provider for too many active downloads and waits for a slot.
	PhaseThrottled
	// PhaseSubmitting is persisted before the provider call; its outcome may be unknown after a crash.
	PhaseSubmitting
	// PhaseProvider is downloading at the provider.
	PhaseProvider
	// PhaseAwaitingLinks is downloaded at the provider, but a file has no link yet.
	PhaseAwaitingLinks
	// PhaseActing runs the post-download action (symlink, local download, season fan-out).
	PhaseActing
	// PhaseComplete is finished and published to the library.
	PhaseComplete
	// PhaseFailed has failed; the Arr sees the failure until the record expires or is retried.
	PhaseFailed
	// PhaseDeleting is being cleaned up; the record is removed last.
	PhaseDeleting
)

// AllPhases lists every phase, for tables and tests.
func AllPhases() []Phase {
	return []Phase{
		PhaseAbsent, PhaseQueued, PhaseThrottled, PhaseSubmitting, PhaseProvider,
		PhaseAwaitingLinks, PhaseActing, PhaseComplete, PhaseFailed, PhaseDeleting,
	}
}

func (p Phase) String() string {
	switch p {
	case PhaseAbsent:
		return "absent"
	case PhaseQueued:
		return "queued"
	case PhaseThrottled:
		return "throttled"
	case PhaseSubmitting:
		return "submitting"
	case PhaseProvider:
		return "provider"
	case PhaseAwaitingLinks:
		return "awaiting_links"
	case PhaseActing:
		return "acting"
	case PhaseComplete:
		return "complete"
	case PhaseFailed:
		return "failed"
	case PhaseDeleting:
		return "deleting"
	}
	return fmt.Sprintf("phase(%d)", uint8(p))
}

// successors is the transition table: the phases p may move to. Staying in a
// phase is not a move and is not listed.
func successors(p Phase) []Phase {
	switch p {
	case PhaseAbsent:
		return []Phase{PhaseQueued, PhaseActing}
	case PhaseQueued:
		return []Phase{PhaseSubmitting, PhaseDeleting}
	case PhaseThrottled:
		return []Phase{PhaseQueued, PhaseDeleting}
	case PhaseSubmitting:
		return []Phase{
			PhaseThrottled, PhaseProvider, PhaseAwaitingLinks, PhaseActing,
			PhaseFailed, PhaseQueued, PhaseDeleting,
		}
	case PhaseProvider:
		return []Phase{PhaseAwaitingLinks, PhaseActing, PhaseFailed, PhaseDeleting}
	case PhaseAwaitingLinks:
		return []Phase{PhaseActing, PhaseFailed, PhaseDeleting}
	case PhaseActing:
		return []Phase{PhaseComplete, PhaseFailed, PhaseDeleting}
	case PhaseComplete:
		return []Phase{PhaseFailed, PhaseDeleting}
	case PhaseFailed:
		return []Phase{PhaseQueued, PhaseActing, PhaseDeleting}
	case PhaseDeleting:
		return []Phase{PhaseAbsent}
	}
	return nil
}
