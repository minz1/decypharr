package manager

import "github.com/sirrobot01/decypharr/internal/state"

type missPhase uint8

const (
	missNone missPhase = iota
	missOnce
	missConfirming
)

func (p missPhase) String() string {
	switch p {
	case missNone:
		return "none"
	case missOnce:
		return "once"
	case missConfirming:
		return "confirming"
	}
	return "miss(?)"
}

func allMissPhases() []missPhase { return []missPhase{missNone, missOnce, missConfirming} }

func missSuccessors(p missPhase) []missPhase {
	switch p {
	case missNone:
		return []missPhase{missOnce}
	case missOnce:
		return []missPhase{missNone, missConfirming}
	case missConfirming:
		return []missPhase{missNone, missConfirming}
	}
	return nil
}

type missState struct {
	phase            missPhase
	recoveryFailures int
}

type missEvent uint8

const (
	missListed missEvent = iota
	missAbsent
	missTruncated
	missFoundOnLookup
	missDropped
	missRecoveryFailed
)

func stepMiss(s missState, ev missEvent) (missState, bool) {
	next := s
	switch ev {
	case missListed:
		next = missState{}
	case missAbsent:
		next.phase = min(s.phase+1, missConfirming)
	case missTruncated:
		return s, true
	case missFoundOnLookup, missDropped:
		if s.phase != missConfirming {
			return s, false
		}
		next = missState{}
	case missRecoveryFailed:
		if s.phase != missConfirming {
			return s, false
		}
		next.recoveryFailures++
	default:
		return s, false
	}
	if next.phase == s.phase && s.phase != missConfirming {
		return next, true
	}
	if !state.Legal(missSuccessors, s.phase, next.phase) {
		return s, false
	}
	return next, true
}
