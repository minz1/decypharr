package keyed_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// counting is a handler with state: Activate numbers the activation, Handle counts the messages of the activation.
type counting struct {
	mu          sync.Mutex
	activations map[string]int
	seen        []countSeen
}

type countState struct {
	activation int
	handled    int
}

type countSeen struct {
	key        string
	activation int
	handled    int
}

func (c *counting) Activate(_ context.Context, key string) countState {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activations[key]++
	return countState{activation: c.activations[key]}
}

func (c *counting) Handle(_ context.Context, key string, st *countState, _ int, _ *keyed.Effects[string, int]) {
	st.handled++
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, countSeen{key: key, activation: st.activation, handled: st.handled})
}

func TestTheRunnerHoldsAKeysStateUntilItsGoroutineExits(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := &counting{activations: map[string]int{}}
		r := keyed.New[string, int, countState](newRoot(t), "state", h, keyed.Options[int]{IdleAfter: time.Second})
		for range 3 {
			if err := r.Send("a", 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Send("b", 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		time.Sleep(5 * time.Second) // both keys go quiet and their goroutines exit
		synctest.Wait()
		if err := r.Send("a", 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		want := []countSeen{
			{"a", 1, 1}, {"a", 1, 2}, {"a", 1, 3}, // one activation, the state carried from message to message
			{"b", 1, 1}, // another key has a state of its own
			{"a", 2, 1}, // after the exit the state is gone: a new activation starts from a fresh one
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		got := slices.Clone(h.seen)
		if !sameSeen(got, want) {
			t.Fatalf("seen %v, want %v", got, want)
		}
	})
}

// sameSeen compares per key, since keys run in parallel and their messages interleave.
func sameSeen(got, want []countSeen) bool {
	for _, key := range []string{"a", "b"} {
		var g, w []countSeen
		for _, s := range got {
			if s.key == key {
				g = append(g, s)
			}
		}
		for _, s := range want {
			if s.key == key {
				w = append(w, s)
			}
		}
		if !slices.Equal(g, w) {
			return false
		}
	}
	return len(got) == len(want)
}
