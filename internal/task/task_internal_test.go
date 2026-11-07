package task

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rs/zerolog"
)

func allStates() []State { return []State{Running, Stopping, Stopped, Failing, Failed} }

// TestTransitionTable pins the lifecycle: running→stopping→stopped and
// running→failing→failed are the only legal moves; every other pair,
// including self-loops, is refused.
func TestTransitionTable(t *testing.T) {
	t.Parallel()
	want := map[[2]State]bool{
		{Running, Stopping}: true,
		{Running, Failing}:  true,
		{Stopping, Stopped}: true,
		{Failing, Failed}:   true,
	}
	for _, from := range allStates() {
		for _, to := range allStates() {
			if got := legal(from, to); got != want[[2]State{from, to}] {
				t.Errorf("legal(%s, %s) = %v, want %v", from, to, got, !got)
			}
		}
	}
}

func TestSettledStateMatchesTable(t *testing.T) {
	t.Parallel()
	for _, closing := range []State{Stopping, Failing} {
		if settled := closing.settled(); !legal(closing, settled) {
			t.Errorf("%s settles to %s, which the table forbids", closing, settled)
		}
	}
}

func TestTransitionRefusesIllegalMove(t *testing.T) {
	t.Parallel()
	group := NewRoot(t.Context(), "root", zerolog.Nop())
	group.mu.Lock()
	moved := group.transition(Stopped)
	state := group.state
	group.mu.Unlock()
	if moved || state != Running {
		t.Fatalf("running→stopped moved=%v state=%s, want refused and still running", moved, state)
	}
}

func childCount(g *Group) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.children)
}

func TestStoppedChildDetachesFromParent(t *testing.T) {
	t.Parallel()
	root := NewRoot(t.Context(), "root", zerolog.Nop())
	child := root.Child("short")
	if err := child.Stop(t.Context()); err != nil {
		t.Fatalf("child stop: %v", err)
	}
	<-child.Done()
	if n := childCount(root); n != 0 {
		t.Fatalf("parent still holds %d stopped children, want 0", n)
	}
	closed := root.Child("idle")
	closed.Close()
	<-closed.Done()
	if n := childCount(root); n != 0 {
		t.Fatalf("parent still holds %d closed children, want 0", n)
	}
	if err := root.Stop(context.Background()); err != nil {
		t.Fatalf("root stop: %v", err)
	}
}

func TestStopNoiseOnlyForPureCancellation(t *testing.T) {
	t.Parallel()
	group := NewRoot(t.Context(), "root", zerolog.Nop())
	if group.stopNoise(context.Canceled) {
		t.Fatal("a cancellation while the group is live counted as stop noise")
	}
	group.Close()
	<-group.Done()
	lost := errors.New("final flush lost 12 records")
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{context.Canceled, true},
		{ErrStopped, true},
		{fmt.Errorf("serve: %w", context.Canceled), true},
		{errors.Join(context.Canceled, ErrStopped), true},
		{errors.Join(lost, context.Canceled), false},
		{lost, false},
		{context.DeadlineExceeded, false},
	} {
		if got := group.stopNoise(tc.err); got != tc.want {
			t.Errorf("stopNoise(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
