package notifications

import (
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestPhaseTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, phaseSuccessors,
		[]phase{phaseIdle, phaseSending, phaseWaiting},
		[]statetest.Edge[phase]{
			{From: phaseIdle, To: phaseSending},
			{From: phaseSending, To: phaseIdle},
			{From: phaseSending, To: phaseWaiting},
			{From: phaseWaiting, To: phaseSending},
			{From: phaseWaiting, To: phaseIdle},
		})
}

func TestTransitionRefusesIllegal(t *testing.T) {
	t.Parallel()
	st := notifierState{phase: phaseIdle}
	if st.transition(phaseWaiting, zerolog.Nop()) {
		t.Fatal("transition(Idle→Waiting) = true, want false")
	}
	if st.phase != phaseIdle {
		t.Fatalf("phase = %v after a refused move, want Idle", st.phase)
	}
	if !st.transition(phaseSending, zerolog.Nop()) || st.phase != phaseSending {
		t.Fatalf("transition(Idle→Sending) did not move, phase %v", st.phase)
	}
}
