package keyed_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// A command is refused only when the mailbox holds nothing it could push out.
func TestACommandIsRefusedAndCountedWhenTheMailboxIsFullOfCommands(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, handled := newKindRunner(t, 2)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2})
		send(t, r, kind{id: 3})

		err := r.Send("a", kind{id: 4})

		if !errors.Is(err, keyed.ErrMailboxFull) {
			t.Fatalf("Send = %v, want ErrMailboxFull", err)
		}
		if got := r.Snapshot().Refused; got != 1 {
			t.Fatalf("refused = %d, want 1", got)
		}
		close(hold)
		synctest.Wait()
		if want := []int{1, 2, 3}; !slices.Equal(*handled, want) {
			t.Fatalf("handled %v, want %v", *handled, want)
		}
	})
}

// Observations are the cheap messages: a mailbox full of them still admits a command, and the oldest is dropped.
func TestAFullMailboxOfObservationsStillAdmitsACommand(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, handled := newKindRunner(t, 2)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2, obs: true})
		send(t, r, kind{id: 3, obs: true})

		send(t, r, kind{id: 4}) // the mailbox is full of observations: the oldest, 2, makes room

		close(hold)
		synctest.Wait()
		if want := []int{1, 3, 4}; !slices.Equal(*handled, want) {
			t.Fatalf("handled %v, want %v", *handled, want)
		}
		info := r.Snapshot()
		if info.Dropped != 1 || info.Refused != 0 {
			t.Fatalf("dropped = %d, refused = %d; want 1 and 0", info.Dropped, info.Refused)
		}
	})
}

func TestAnAskAlsoPushesOutTheOldestObservation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hold := make(chan struct{})
		var handled []int
		h := keyed.HandlerFunc[string, kind](
			func(_ context.Context, _ string, m kind, fx *keyed.Effects[string, kind]) {
				if m.hold != nil {
					<-m.hold
				}
				handled = append(handled, m.id)
				fx.Reply(m.id)
			},
		)
		r := keyed.New[string, kind, struct{}](newRoot(t), "evict-ask", h, keyed.Options[kind]{
			MailboxSize: 1,
			Class:       classify,
		})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2, obs: true})

		answer := make(chan any, 1)
		go func() {
			got, err := r.Ask(t.Context(), "a", kind{id: 3})
			if err != nil {
				t.Error(err)
			}
			answer <- got
		}()
		synctest.Wait()
		close(hold)
		synctest.Wait()

		if got := <-answer; got != 3 {
			t.Fatalf("Ask = %v, want 3", got)
		}
		if want := []int{1, 3}; !slices.Equal(handled, want) {
			t.Fatalf("handled %v, want %v: the observation made room", handled, want)
		}
	})
}

// Eviction never touches what bypasses the bound: a timer or a child's result is not an observation to give up.
func TestEvictionLeavesBypassMessagesAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, handled := newKindRunner(t, 1)
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		r.After("a", 0, kind{id: 2, obs: true}) // an observation by class, but queued by a timer: it bypasses
		synctest.Wait()

		send(t, r, kind{id: 3}) // room: only the bypass message is queued
		err := r.Send("a", kind{id: 4})

		if !errors.Is(err, keyed.ErrMailboxFull) {
			t.Fatalf("Send = %v, want ErrMailboxFull: the timer's message is not droppable", err)
		}
		close(hold)
		synctest.Wait()
		if want := []int{1, 2, 3}; !slices.Equal(*handled, want) {
			t.Fatalf("handled %v, want %v", *handled, want)
		}
	})
}
