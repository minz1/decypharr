package rclone

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestRcdTransitionTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, rcdSuccessors,
		[]rcdState{rcdNew, rcdStarting, rcdReady, rcdRestarting, rcdStopping, rcdStopped, rcdFailed},
		[]statetest.Edge[rcdState]{
			{From: rcdNew, To: rcdStarting},
			{From: rcdNew, To: rcdStopped},
			{From: rcdStarting, To: rcdReady},
			{From: rcdStarting, To: rcdRestarting},
			{From: rcdStarting, To: rcdFailed},
			{From: rcdStarting, To: rcdStopping},
			{From: rcdReady, To: rcdRestarting},
			{From: rcdReady, To: rcdFailed},
			{From: rcdReady, To: rcdStopping},
			{From: rcdRestarting, To: rcdStarting},
			{From: rcdRestarting, To: rcdStopping},
			{From: rcdFailed, To: rcdStopping},
			{From: rcdStopping, To: rcdStopped},
		})
}

func TestRcdStateNames(t *testing.T) {
	t.Parallel()
	want := map[rcdState]string{
		rcdNew: "new", rcdStarting: "starting", rcdReady: "ready", rcdRestarting: "restarting",
		rcdStopping: "stopping", rcdStopped: "stopped", rcdFailed: "failed",
	}
	for s, name := range want {
		if got := s.String(); got != name {
			t.Errorf("state %d = %q, want %q", uint32(s), got, name)
		}
	}
}
