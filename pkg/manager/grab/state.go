package grab

import (
	"errors"
	"fmt"
	"slices"
	"time"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// Failure says why and when a grab failed.
type Failure struct {
	Reason string
	Cause  string
	At     time.Time
	Count  int
}

// Cleanup is what a Deleting grab's cleanup does. It is persisted so a restart
// resumes the same cleanup. The zero value removes only the queue record and
// the staged NZB: never files, never the provider's torrent.
type Cleanup struct {
	DeleteFiles   bool
	ReleaseDelete bool
}

// SubmitAttempt records the provider call that Submitting journals before it is made.
type SubmitAttempt struct {
	Provider string
	At       time.Time
}

// State is one grab as the reducer sees it.
type State struct {
	Key        string
	Phase      Phase
	PhaseSince time.Time
	// PackKey links a season child to its pack; a season child is never submitted to a provider.
	PackKey string
	NZB     bool
	// NoSubmit, when set, is why a Submit without a PackKey is refused.
	NoSubmit      string
	Failure       Failure
	Cleanup       Cleanup
	SubmitAttempt SubmitAttempt
	// UnknownRaw is the provider's last unrecognised status, shown as is.
	UnknownRaw string
	// StalledSince is when the first of the polls in a row that saw the placement stalled did; zero when the last
	// poll did not.
	StalledSince  time.Time
	ProviderPhase debridTypes.Phase
}

// season reports whether the grab is a season child of a pack.
func (s State) season() bool { return s.PackKey != "" }

// Validate checks the invariants every reachable state holds.
func (s State) Validate() error {
	if !slices.Contains(AllPhases(), s.Phase) {
		return fmt.Errorf("unknown phase %v", s.Phase)
	}
	if s.season() && submitsToProvider(s.Phase) {
		return fmt.Errorf("season child %q in %v", s.Key, s.Phase)
	}
	if s.ProviderPhase != debridTypes.PhaseNone && s.Phase != PhaseProvider {
		return fmt.Errorf("provider phase %v in %v", s.ProviderPhase, s.Phase)
	}
	if s.Phase == PhaseFailed {
		if s.Failure.Reason == "" {
			return errors.New("failed without a reason")
		}
		if s.Failure.At.IsZero() {
			return errors.New("failed without a time")
		}
	}
	return nil
}

// submitsToProvider reports the phases that only a grab sent to a provider reaches.
func submitsToProvider(p Phase) bool {
	switch p {
	case PhaseQueued, PhaseThrottled, PhaseSubmitting, PhaseProvider, PhaseAwaitingLinks:
		return true
	case PhaseAbsent, PhaseActing, PhaseComplete, PhaseFailed, PhaseDeleting:
	}
	return false
}
