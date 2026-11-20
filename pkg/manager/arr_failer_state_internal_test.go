package manager

import (
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestFailItemTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, failItemSuccessors, []failItemPhase{failWaiting, failAckWaiting, failRunning},
		[]statetest.Edge[failItemPhase]{
			{From: failWaiting, To: failRunning},
			{From: failAckWaiting, To: failRunning},
			{From: failRunning, To: failWaiting},
			{From: failRunning, To: failAckWaiting},
		})
}

func TestFailItemMoveRefusesAnIllegalEdge(t *testing.T) {
	t.Parallel()
	it := &failItem{phase: failWaiting}
	err := it.move(failAckWaiting)
	if _, ok := errors.AsType[*transitionRefusedError](err); !ok || it.phase != failWaiting {
		t.Fatalf("move = %v, phase %v; want a refusal and the phase unchanged", err, it.phase)
	}
	if err = it.move(failRunning); err != nil || it.phase != failRunning {
		t.Fatalf("move = %v, phase %v; want running", err, it.phase)
	}
}
