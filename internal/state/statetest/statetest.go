// Package statetest checks a state machine's transition table.
package statetest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/sirrobot01/decypharr/internal/state"
)

// Edge is one legal move.
type Edge[S comparable] struct {
	From, To S
}

// Table checks every (from, to) pair over all: exactly the edges listed are
// legal and every other pair, self-loops included, is refused. It also fails
// if the table names a state outside all, so a new state cannot be added
// without updating the test.
func Table[S comparable](t *testing.T, successors state.Successors[S], all []S, legal []Edge[S]) {
	t.Helper()
	want := make(map[Edge[S]]bool, len(legal))
	for _, edge := range legal {
		want[edge] = true
	}
	for _, from := range all {
		for _, to := range successors(from) {
			if !slices.Contains(all, to) {
				t.Errorf("%v may move to %v, which is not in the state list", from, to)
			}
		}
		for _, to := range all {
			edge := Edge[S]{From: from, To: to}
			if got := state.Legal(successors, from, to); got != want[edge] {
				t.Errorf("%s: legal = %v, want %v", describe(edge), got, want[edge])
			}
		}
	}
	for edge := range want {
		if !slices.Contains(all, edge.From) || !slices.Contains(all, edge.To) {
			t.Errorf("%s names a state that is not in the state list", describe(edge))
		}
	}
}

func describe[S comparable](edge Edge[S]) string {
	return fmt.Sprintf("%v → %v", edge.From, edge.To)
}
