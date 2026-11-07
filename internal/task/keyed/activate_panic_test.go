package keyed_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// flaky is a handler whose first Activate panics, as a state load can. Its zero state would be a lie (for a
// holder, "nothing holds it"), so Handle must never see it.
type flaky struct {
	mu        sync.Mutex
	activates int
	gate      chan struct{} // the first Activate waits for it, so messages queue behind it
	handled   []int
	states    []int
}

func (f *flaky) Activate(context.Context, string) int {
	f.mu.Lock()
	f.activates++
	n := f.activates
	f.mu.Unlock()
	if n == 1 {
		<-f.gate
		panic("cannot load")
	}
	return 100 + n // the state of a good activation
}

func (f *flaky) Handle(_ context.Context, _ string, st *int, m int, fx *keyed.Effects[string, int]) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handled = append(f.handled, m)
	f.states = append(f.states, *st)
	fx.Reply(m)
}

func TestAnActivatePanicHandlesNothingInThatActivation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := &flaky{gate: make(chan struct{})}
		logs := &syncBuffer{}
		r := keyed.New[string, int, int](newRoot(t), "flaky", h, keyed.Options[int]{Logger: zerolog.New(logs)})
		if err := r.Send("a", 1); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // Activate is waiting at its gate
		if err := r.Send("a", 2); err != nil {
			t.Fatal(err)
		}
		r.After("a", 0, 3) // a timer's message queues too
		asked := make(chan error, 1)
		go func() {
			_, err := r.Ask(t.Context(), "a", 4)
			asked <- err
		}()
		synctest.Wait()

		close(h.gate) // Activate panics
		synctest.Wait()

		if err := <-asked; !errors.Is(err, keyed.ErrHandlerPanicked) {
			t.Fatalf("the queued Ask = %v, want ErrHandlerPanicked", err)
		}
		h.mu.Lock()
		handled := slices.Clone(h.handled)
		h.mu.Unlock()
		if len(handled) != 0 {
			t.Fatalf("Handle ran for %v on the fallback state, want it never called", handled)
		}
		info := r.Snapshot()
		if info.Dropped != 4 || info.Panics != 1 {
			t.Fatalf("dropped = %d, panics = %d; want the 4 queued messages dropped and the panic counted",
				info.Dropped, info.Panics)
		}
		if len(info.Keys) != 0 {
			t.Fatalf("keys = %+v, want the key gone: its goroutine exited", info.Keys)
		}
		// The timer's message is dropped with the rest; a dropped deadline must be visible in the log.
		if !strings.Contains(logs.String(), `"timers":1`) {
			t.Fatalf("the dropped timer message was not logged: %s", logs.String())
		}

		// The next message activates the key again, and now Handle sees a real state.
		if err := r.Send("a", 5); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		h.mu.Lock()
		defer h.mu.Unlock()
		if !slices.Equal(h.handled, []int{5}) || !slices.Equal(h.states, []int{102}) {
			t.Fatalf(
				"handled %v with states %v, want [5] with the second activation's state [102]",
				h.handled,
				h.states,
			)
		}
	})
}
