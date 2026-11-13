package manager

import (
	"slices"
	"testing"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

type sentEvent struct {
	key  string
	kind grab.EventKind
}

// fakeSend records every event; keys in refuse get err instead.
type fakeSend struct {
	sent   []sentEvent
	refuse map[string]error
}

func (f *fakeSend) send(key string, ev grab.Event) error {
	if err := f.refuse[key]; err != nil {
		return err
	}
	f.sent = append(f.sent, sentEvent{key, ev.Kind})
	return nil
}

func (f *fakeSend) keys(kind grab.EventKind) []string {
	var out []string
	for _, s := range f.sent {
		if s.kind == kind {
			out = append(out, s.key)
		}
	}
	return out
}

func newTestAdmission(capacity int) (*admission, *fakeSend) {
	f := &fakeSend{refuse: map[string]error{}}
	return newAdmission(capacity, f.send), f
}

func TestAdmissionGrantsUpToTheCapInOrder(t *testing.T) {
	t.Parallel()
	a, f := newTestAdmission(2)
	for _, k := range []string{"a", "b", "c", "d"} {
		a.Request(k)
	}
	if got := f.keys(grab.EvAdmitted); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("admitted %v, want [a b]", got)
	}
	a.Release("b")
	a.Release("a")
	if got := f.keys(grab.EvAdmitted); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("admitted %v, want [a b c d]", got)
	}
}

func TestAdmissionRequestIsIdempotentPerKey(t *testing.T) {
	t.Parallel()
	a, f := newTestAdmission(1)
	a.Request("a")
	a.Request("a")
	a.Request("b")
	a.Request("b")
	a.Release("a")
	a.Release("b")
	if got := f.keys(grab.EvAdmitted); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("admitted %v, want [a b]", got)
	}
}

func TestCancelledRequestNeverAdmits(t *testing.T) {
	t.Parallel()
	a, f := newTestAdmission(1)
	a.Request("a")
	a.Request("b")
	a.Request("c")
	a.Cancel("b")
	a.Release("a")
	if got := f.keys(grab.EvAdmitted); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("admitted %v, want [a c]", got)
	}
}

func TestSlotFreedWakesOnlyThatProvidersThrottledKeys(t *testing.T) {
	t.Parallel()
	a, f := newTestAdmission(5)
	a.Throttled("a", []string{"rd"})
	a.Throttled("b", []string{"tb"})
	a.Throttled("c", []string{"rd", "tb"})
	a.Throttled("a", []string{"rd"})
	a.SlotFreed("rd")
	if got := f.keys(grab.EvSlotFreed); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("woken %v, want [a c]", got)
	}
	a.SlotFreed("rd")
	if got := f.keys(grab.EvSlotFreed); len(got) != 2 {
		t.Fatalf("a second SlotFreed woke again: %v", got)
	}
}

func TestRefusedGrantReturnsTheSlot(t *testing.T) {
	t.Parallel()
	a, f := newTestAdmission(1)
	f.refuse["a"] = keyed.ErrMailboxFull
	a.Request("a")
	a.Request("b")
	if got := f.keys(grab.EvAdmitted); len(got) != 0 {
		t.Fatalf("admitted %v while a stays at the head", got)
	}
	delete(f.refuse, "a")
	a.Request("a") // a's own deadline timer re-requests it
	if got := f.keys(grab.EvAdmitted); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("admitted %v, want [a]", got)
	}
	a.Release("a")
	if got := f.keys(grab.EvAdmitted); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("admitted %v, want [a b]", got)
	}
}

func TestReleaseOfAKeyNotHoldingIsANoop(t *testing.T) {
	t.Parallel()
	a, f := newTestAdmission(1)
	a.Request("a")
	a.Request("b")
	a.Release("b") // waiting, not holding
	a.Release("x")
	if got := f.keys(grab.EvAdmitted); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("admitted %v, want [a]", got)
	}
}
