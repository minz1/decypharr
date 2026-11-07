package keyed

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestRunnerStateTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, runSuccessors,
		[]runState{runRunning, runStopping, runStopped},
		[]statetest.Edge[runState]{
			{From: runRunning, To: runStopping},
			{From: runStopping, To: runStopped},
		})
}
