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

// Stop lets the message in flight finish and drops everything queued behind it, whatever its kind: commands,
// observations, a timer's message, and an Ask, whose asker is told the outcome is unknown. Nothing queued is
// handled once Stop has begun, even though the handler is released only afterwards.
func TestStopDropsEveryKindOfQueuedMessage(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := newRoot(t)
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
		r := keyed.New[string, kind, struct{}](g, "stop-queue", h, keyed.Options[kind]{
			MailboxSize: 8,
			Class:       classify,
		})
		hold := make(chan struct{})
		send(t, r, kind{id: 1, hold: hold})
		synctest.Wait()
		send(t, r, kind{id: 2})
		send(t, r, kind{id: 3, obs: true})
		r.After("a", time.Second, kind{id: 4})
		time.Sleep(2 * time.Second) // the timer fires into the queue behind the blocked handler
		asked := make(chan error, 1)
		go func() {
			_, err := r.Ask(t.Context(), "a", kind{id: 5})
			asked <- err
		}()
		synctest.Wait()
		if depth := r.Snapshot().Keys[0].Depth; depth != 4 {
			t.Fatalf("depth = %d, want 4 queued behind the handler", depth)
		}

		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			stop(t, g)
		}()
		synctest.Wait() // the Stop has begun and waits for the blocked handler
		close(hold)     // the handler is released only now
		<-stopped

		if want := []int{1}; !slices.Equal(handled, want) {
			t.Fatalf("handled %v, want only the message in flight: %v", handled, want)
		}
		err := <-asked
		if !errors.Is(err, keyed.ErrOutcomeUnknown) || !errors.Is(err, keyed.ErrStopped) {
			t.Fatalf("the queued Ask = %v, want ErrOutcomeUnknown wrapping ErrStopped", err)
		}
	})
}
