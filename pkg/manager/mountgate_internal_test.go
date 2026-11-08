package manager

import (
	"bytes"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
	"github.com/sirrobot01/decypharr/internal/task"
)

func TestMountGateTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, mountGateSuccessors,
		[]mountGateState{mountGateServing, mountGateStopping},
		[]statetest.Edge[mountGateState]{{From: mountGateServing, To: mountGateStopping}})
}

func TestMountGateRefusesStoppingToStopping(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	g := NewMountGate(zerolog.New(&logs))
	g.markStopping()
	if logs.Len() != 0 {
		t.Fatalf("a legal move logged: %s", logs.String())
	}
	if g.transition(mountGateStopping, mountGateStopping) {
		t.Fatal("Stopping -> Stopping was allowed")
	}
	if !bytes.Contains(logs.Bytes(), []byte("Refused an illegal mount gate move")) {
		t.Fatalf("the refusal was not logged: %q", logs.String())
	}
	if !g.IsStopping() {
		t.Fatal("the gate left Stopping")
	}
}

func TestMountGateMarkStoppingTwiceIsSilent(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	g := NewMountGate(zerolog.New(&logs))
	if g.IsStopping() {
		t.Fatal("a new gate is stopping")
	}
	select {
	case <-g.Stopping():
		t.Fatal("a new gate's channel is closed")
	default:
	}
	g.markStopping()
	g.markStopping() // a second close would panic
	select {
	case <-g.Stopping():
	default:
		t.Fatal("the channel is open after markStopping")
	}
	if logs.Len() != 0 {
		t.Fatalf("markStopping logged: %s", logs.String())
	}
}

func TestMountGateNilNeverCloses(t *testing.T) {
	t.Parallel()
	var g *MountGate
	g.markStopping()
	if g.IsStopping() {
		t.Fatal("a nil gate is stopping")
	}
	select {
	case <-g.Stopping():
		t.Fatal("a nil gate's channel fired")
	default:
	}
}

func TestManagerStopClosesItsGate(t *testing.T) {
	t.Parallel()
	m, _ := newLifecycleManager(t)
	if m.mountGate == nil || m.mountGate.IsStopping() {
		t.Fatal("a new manager needs an open default gate")
	}
	custom := NewMountGate(zerolog.Nop())
	m.SetMountGate(custom)
	m.SetMountGate(nil)
	if m.mountGate != custom {
		t.Fatal("SetMountGate(nil) replaced the gate")
	}
	ctx, cancel := task.Detached(t.Context(), testStopTimeout)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !custom.IsStopping() {
		t.Fatal("Manager.Stop did not close the gate")
	}
}
