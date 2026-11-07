// Package state holds the shared pieces of an explicit state machine.
//
// A machine in this codebase is a typed state, a transition table and one
// transition function that refuses every move the table does not list. The
// table is a successors function rather than a map, so it is data without
// being a package-level variable: for each state it returns the states that
// state may move to.
//
// Use Legal inside a machine's own transition function when its state is
// guarded by a mutex, and Atomic for a state that hot paths change with
// compare-and-swap. The statetest package checks a table pair by pair.
package state

import (
	"slices"
	"sync/atomic"
)

// Successors is a transition table: it returns the states s may move to.
type Successors[S comparable] func(s S) []S

// Legal reports whether the table allows a move from one state to another.
func Legal[S comparable](successors Successors[S], from, to S) bool {
	return slices.Contains(successors(from), to)
}

// Atomic is a machine state held in one atomic word, for states that hot
// paths change without a lock. Transition refuses moves the table does not
// list and counts the refusals, so a test or a /debug view can show that no
// illegal move was attempted. The zero value is not usable: use NewAtomic.
type Atomic[S ~uint32] struct {
	value      atomic.Uint32
	refused    atomic.Uint64
	successors Successors[S]
}

// NewAtomic returns an Atomic in the initial state.
func NewAtomic[S ~uint32](initial S, successors Successors[S]) *Atomic[S] {
	machine := &Atomic[S]{successors: successors}
	machine.value.Store(uint32(initial))
	return machine
}

// Load returns the current state.
func (m *Atomic[S]) Load() S { return S(m.value.Load()) }

// Transition moves the state from one value to another in a single
// compare-and-swap. It returns false, changing nothing, when the current
// state is not from (another goroutine moved first) or when the table does
// not allow the move; only the second counts as a refusal.
func (m *Atomic[S]) Transition(from, to S) bool {
	if !Legal(m.successors, from, to) {
		m.refused.Add(1)
		return false
	}
	return m.value.CompareAndSwap(uint32(from), uint32(to))
}

// Refused reports how many illegal moves were attempted.
func (m *Atomic[S]) Refused() uint64 { return m.refused.Load() }
