package dfs

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestDFSTransitionTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, dfsSuccessors,
		[]dfsState{dfsNew, dfsStarting, dfsReady, dfsFailed, dfsStopping, dfsStopped},
		[]statetest.Edge[dfsState]{
			{From: dfsNew, To: dfsStarting},
			{From: dfsNew, To: dfsStopped},
			{From: dfsStarting, To: dfsReady},
			{From: dfsStarting, To: dfsFailed},
			{From: dfsStarting, To: dfsStopping},
			{From: dfsReady, To: dfsFailed},
			{From: dfsReady, To: dfsStopping},
			{From: dfsFailed, To: dfsStopping},
			{From: dfsStopping, To: dfsStopped},
		})
}

func TestDFSStateNames(t *testing.T) {
	t.Parallel()
	want := map[dfsState]string{
		dfsNew: "new", dfsStarting: "starting", dfsReady: "ready",
		dfsFailed: "failed", dfsStopping: "stopping", dfsStopped: "stopped",
	}
	for s, name := range want {
		if got := s.String(); got != name {
			t.Errorf("state %d = %q, want %q", uint32(s), got, name)
		}
	}
}
