package keyed_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// kind says how a test message is classified; same names the observation's coalesce key.
type kind struct {
	id   int
	obs  bool
	same string
	tick bool
	hold <-chan struct{}
}

func classify(m kind) keyed.Class {
	switch {
	case m.tick:
		return keyed.Timer
	case m.obs:
		return keyed.Observation
	default:
		return keyed.Command
	}
}

func newKindRunner(t *testing.T, size int) (*keyed.Runner[string, kind], *[]int) {
	t.Helper()
	var handled []int
	h := keyed.HandlerFunc[string, kind](func(_ context.Context, _ string, m kind, _ *keyed.Effects[string, kind]) {
		if m.hold != nil {
			<-m.hold
		}
		handled = append(handled, m.id)
	})
	r := keyed.New[string, kind](newRoot(t), "overflow", h, keyed.Options[kind]{
		MailboxSize: size,
		Class:       classify,
		CoalesceKey: func(m kind) string { return m.same },
	})
	return r, &handled
}

func TestObservationsWithTheSameKeyCoalesceInPlaceLatestWins(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, handled := newKindRunner(t, 2)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()

		send(t, r, kind{id: 2, obs: true, same: "x"})
		send(t, r, kind{id: 3}) // a command after it
		send(t, r, kind{id: 4, obs: true, same: "x"})
		send(t, r, kind{id: 5, obs: true, same: "x"}) // coalesces even though the mailbox is full

		if depth := r.Snapshot().Keys[0].Depth; depth != 2 {
			t.Fatalf("depth = %d, want 2: the three observations are one", depth)
		}
		close(hold)
		synctest.Wait()
		if want := []int{1, 5, 3}; !slices.Equal(*handled, want) {
			t.Fatalf("handled %v, want %v: the latest observation, in the place of the first", *handled, want)
		}
		if got := r.Snapshot().Coalesced; got != 2 {
			t.Fatalf("coalesced = %d, want 2", got)
		}
	})
}

func TestAFullMailboxDropsTheOldestObservationWithoutACoalesceKey(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, handled := newKindRunner(t, 2)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2, obs: true})
		send(t, r, kind{id: 3})

		send(t, r, kind{id: 4, obs: true}) // full: the oldest observation, 2, makes room

		close(hold)
		synctest.Wait()
		if want := []int{1, 3, 4}; !slices.Equal(*handled, want) {
			t.Fatalf("handled %v, want %v", *handled, want)
		}
		if got := r.Snapshot().Dropped; got != 1 {
			t.Fatalf("dropped = %d, want 1", got)
		}
	})
}

func TestAnObservationIsRefusedWhenTheMailboxHoldsNothingToDrop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, handled := newKindRunner(t, 1)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2})

		err := r.Send("a", kind{id: 3, obs: true})

		if !errors.Is(err, keyed.ErrMailboxFull) {
			t.Fatalf("Send = %v, want ErrMailboxFull", err)
		}
		close(hold)
		synctest.Wait()
		if want := []int{1, 2}; !slices.Equal(*handled, want) {
			t.Fatalf("handled %v, want %v", *handled, want)
		}
	})
}

func TestACommandIsRefusedAndCountedWhenTheMailboxIsFull(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, _ := newKindRunner(t, 1)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2, obs: true})

		err := r.Send("a", kind{id: 3})

		if !errors.Is(err, keyed.ErrMailboxFull) {
			t.Fatalf("Send = %v, want ErrMailboxFull: a command does not push an observation out", err)
		}
		if got := r.Snapshot().Refused; got != 1 {
			t.Fatalf("refused = %d, want 1", got)
		}
		close(hold)
	})
}

func TestATimerClassMessageIsNeverRefused(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, handled := newKindRunner(t, 1)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2})

		send(t, r, kind{id: 3, tick: true})
		send(t, r, kind{id: 4, tick: true})

		close(hold)
		synctest.Wait()
		if want := []int{1, 2, 3, 4}; !slices.Equal(*handled, want) {
			t.Fatalf("handled %v, want %v", *handled, want)
		}
	})
}

func TestACoalescedObservationKeepsItsPlaceAndItsAge(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, _ := newKindRunner(t, 4)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2, obs: true, same: "x"})
		time.Sleep(5 * time.Second)

		send(t, r, kind{id: 3, obs: true, same: "x"})

		if age := r.Snapshot().Keys[0].OldestAge; age != 5*time.Second {
			t.Fatalf("oldest age = %v, want 5s: the queued place is the one that waited", age)
		}
		close(hold)
	})
}

func send(t *testing.T, r *keyed.Runner[string, kind], m kind) {
	t.Helper()
	if err := r.Send("a", m); err != nil {
		t.Fatalf("Send(%d) = %v", m.id, err)
	}
}
