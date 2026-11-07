package config

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestPendingTransitionTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, Pending.successors,
		[]Pending{PendingNone, PendingRestart, PendingFailed},
		[]statetest.Edge[Pending]{
			{From: PendingNone, To: PendingRestart},
			{From: PendingRestart, To: PendingNone},
			{From: PendingRestart, To: PendingFailed},
			{From: PendingFailed, To: PendingRestart},
			{From: PendingFailed, To: PendingNone},
		})
}
