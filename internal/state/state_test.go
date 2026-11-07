package state_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

type phase uint32

const (
	idle phase = iota + 1
	busy
	done
)

func (p phase) String() string {
	switch p {
	case idle:
		return "idle"
	case busy:
		return "busy"
	case done:
		return "done"
	}
	return "unknown"
}

func successors(p phase) []phase {
	switch p {
	case idle:
		return []phase{busy, done}
	case busy:
		return []phase{idle, done}
	case done:
		return nil
	}
	return nil
}

func TestTableHelperAcceptsTheTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, successors, []phase{idle, busy, done}, []statetest.Edge[phase]{
		{From: idle, To: busy}, {From: idle, To: done}, {From: busy, To: idle}, {From: busy, To: done},
	})
}

func TestTableHelperCatchesAMismatch(t *testing.T) {
	t.Parallel()
	inner := &testing.T{}
	statetest.Table(inner, successors, []phase{idle, busy, done}, []statetest.Edge[phase]{
		{From: idle, To: busy},
	})
	if !inner.Failed() {
		t.Fatal("a table with unlisted legal edges passed the helper")
	}
}

func TestAtomicRefusesIllegalMovesAndCountsThem(t *testing.T) {
	t.Parallel()
	machine := state.NewAtomic(idle, successors)
	if !machine.Transition(idle, done) {
		t.Fatal("idle→done refused")
	}
	if machine.Transition(done, idle) {
		t.Fatal("done→idle allowed")
	}
	if machine.Load() != done || machine.Refused() != 1 {
		t.Fatalf("state=%v refused=%d, want done and 1", machine.Load(), machine.Refused())
	}
}

func TestAtomicLostRaceIsNotARefusal(t *testing.T) {
	t.Parallel()
	machine := state.NewAtomic(idle, successors)
	if !machine.Transition(idle, busy) {
		t.Fatal("idle→busy refused")
	}
	if machine.Transition(idle, done) {
		t.Fatal("a stale from-state moved the machine")
	}
	if machine.Refused() != 0 {
		t.Fatalf("refused = %d, want 0: losing a race is not an illegal move", machine.Refused())
	}
}

func TestAtomicOneWinnerPerMove(t *testing.T) {
	t.Parallel()
	machine := state.NewAtomic(idle, successors)
	var wins atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if machine.Transition(idle, busy) {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 || machine.Load() != busy {
		t.Fatalf("wins=%d state=%v, want exactly one winner and busy", wins.Load(), machine.Load())
	}
}
