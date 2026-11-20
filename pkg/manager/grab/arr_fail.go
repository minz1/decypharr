package grab

import (
	"errors"
	"fmt"

	"github.com/sirrobot01/decypharr/internal/state"
)

type ArrFailState uint8

const (
	ArrFailNone ArrFailState = iota
	ArrFailPending
	ArrFailQueued
	ArrFailSkipped
)

func (s ArrFailState) String() string {
	switch s {
	case ArrFailNone:
		return ""
	case ArrFailPending:
		return "pending"
	case ArrFailQueued:
		return "queued"
	case ArrFailSkipped:
		return "skipped"
	}
	return fmt.Sprintf("arr_fail(%d)", uint8(s))
}

func arrFailNamed(s string) ArrFailState {
	if s == "" {
		return ArrFailNone
	}
	for _, known := range []ArrFailState{ArrFailPending, ArrFailQueued} {
		if known.String() == s {
			return known
		}
	}
	return ArrFailSkipped
}

func arrFailSuccessors(s ArrFailState) []ArrFailState {
	switch s {
	case ArrFailNone:
		return []ArrFailState{ArrFailPending}
	case ArrFailPending:
		return []ArrFailState{ArrFailQueued, ArrFailNone}
	case ArrFailQueued, ArrFailSkipped:
		return []ArrFailState{ArrFailNone}
	}
	return nil
}

type ArrFail struct {
	State ArrFailState
	Arr   string
	JobID string
}

func (a ArrFail) to(next ArrFailState) (ArrFail, error) {
	if !state.Legal(arrFailSuccessors, a.State, next) {
		return a, fmt.Errorf("arr fail %q -> %q is not a move", a.State, next)
	}
	if next == ArrFailNone {
		return ArrFail{}, nil
	}
	a.State = next
	return a, nil
}

func (s State) validArrFail() error {
	a := s.ArrFail
	if a.State == ArrFailNone {
		if a.Arr != "" || a.JobID != "" {
			return errors.New("arr fail data without a state")
		}
		return nil
	}
	switch {
	case s.Phase != PhaseFailed:
		return fmt.Errorf("arr fail %q in %v", a.State, s.Phase)
	case s.NZB || s.season():
		return fmt.Errorf("arr fail %q on a grab the Arr does not own", a.State)
	case a.Arr == "":
		return fmt.Errorf("arr fail %q without an Arr", a.State)
	case a.JobID != "" && a.State != ArrFailQueued:
		return fmt.Errorf("arr fail %q carries a job", a.State)
	}
	return nil
}

func arrFailQueued(st State, ev Event) (State, []Effect, error) {
	if st.ArrFail.State != ArrFailPending || !ev.Occurrence.Equal(st.Failure.At) {
		return refuse(st, ev, "no such pending fail")
	}
	next, err := st.ArrFail.to(ArrFailQueued)
	if err != nil {
		return refuse(st, ev, err.Error())
	}
	next.JobID = ev.JobID
	st.ArrFail = next
	return st, []Effect{{Kind: FxPersist}}, nil
}

func arrFailDone(st State, ev Event) (State, []Effect, error) {
	switch {
	case st.Phase != PhaseFailed && st.Phase != PhaseComplete:
		return refuse(st, ev, notAccepted)
	case st.Phase == PhaseFailed && st.ArrFail.Arr != "" && st.ArrFail.Arr != ev.Arr:
		return refuse(st, ev, "queued for another Arr")
	case st.Phase == PhaseComplete && st.PhaseSince.After(ev.JobCreated):
		return refuse(st, ev, "completed after the job was queued")
	}
	return remove(st, ev, Cleanup{DeleteFiles: true})
}
