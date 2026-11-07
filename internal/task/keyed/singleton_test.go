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

// counter is a singleton's handler: its state is a running total, which must last as long as the singleton does.
type counter struct{}

func (counter) Activate(context.Context, struct{}) int { return 0 }

func (counter) Handle(_ context.Context, _ struct{}, total *int, add int, fx *keyed.Effects[struct{}, int]) {
	*total += add
	fx.Reply(*total)
}

func newCounter(t *testing.T) *keyed.Singleton[int, int] {
	t.Helper()
	return keyed.NewSingleton[int, int](newRoot(t), "counter", counter{}, keyed.Options[int]{IdleAfter: time.Second})
}

func TestASingletonStaysUpAndKeepsItsStateThroughIdleTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := newCounter(t)
		if got, err := s.Ask(t.Context(), 5); err != nil || got != 5 {
			t.Fatalf("Ask = %v, %v; want 5", got, err)
		}

		time.Sleep(time.Hour)
		synctest.Wait()

		if got, err := s.Ask(t.Context(), 2); err != nil || got != 7 {
			t.Fatalf("Ask after an hour = %v, %v; want 7: the state outlives idle time", got, err)
		}
		if info := s.Snapshot(); len(info.Keys) != 1 || !info.Keys[0].Running {
			t.Fatalf("snapshot = %+v, want the one key still running", info)
		}
	})
}

func TestASingletonSendsAndRunsItsTimers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		got := make(chan int, 8)
		h := keyed.HandlerFunc[struct{}, int](
			func(_ context.Context, _ struct{}, m int, _ *keyed.Effects[struct{}, int]) {
				got <- m
			},
		)
		s := keyed.NewSingleton[int, struct{}](newRoot(t), "timers", h, keyed.Options[int]{})
		if err := s.Send(1); err != nil {
			t.Fatal(err)
		}
		s.After(time.Second, 2)
		cancelled := s.After(2*time.Second, 3)
		s.CancelTimer(cancelled)

		time.Sleep(time.Minute)
		synctest.Wait()

		var seen []int
		for len(got) > 0 {
			seen = append(seen, <-got)
		}
		if !slices.Equal(seen, []int{1, 2}) {
			t.Fatalf("handled %v, want [1 2]", seen)
		}
	})
}

func TestASingletonRefusesAfterStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := newRoot(t)
		s := keyed.NewSingleton[int, int](g, "stopped", counter{}, keyed.Options[int]{})
		stop(t, g)

		if err := s.Send(1); !errors.Is(err, keyed.ErrStopped) {
			t.Fatalf("Send = %v, want ErrStopped", err)
		}
		if _, err := s.Ask(t.Context(), 1); !errors.Is(err, keyed.ErrStopped) {
			t.Fatalf("Ask = %v, want ErrStopped", err)
		}
		if id := s.After(time.Second, 1); id != 0 {
			t.Fatalf("After = %d, want no timer", id)
		}
		if got := s.Snapshot().State; got != "stopped" {
			t.Fatalf("state = %q, want stopped", got)
		}
	})
}
