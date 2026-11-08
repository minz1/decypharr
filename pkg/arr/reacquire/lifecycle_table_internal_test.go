package reacquire

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestServiceLifecycleTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, serviceSuccessors,
		[]serviceState{serviceConstructed, serviceRunning, serviceFailed, serviceStopped},
		[]statetest.Edge[serviceState]{
			{From: serviceConstructed, To: serviceRunning},
			{From: serviceConstructed, To: serviceFailed},
			{From: serviceConstructed, To: serviceStopped},
			{From: serviceRunning, To: serviceStopped},
			{From: serviceFailed, To: serviceStopped},
		})
}
