package grab

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestPhaseTable(t *testing.T) {
	t.Parallel()
	edges := func(from Phase, to ...Phase) []statetest.Edge[Phase] {
		out := make([]statetest.Edge[Phase], 0, len(to))
		for _, p := range to {
			out = append(out, statetest.Edge[Phase]{From: from, To: p})
		}
		return out
	}
	var legal []statetest.Edge[Phase]
	legal = append(legal, edges(PhaseAbsent, PhaseQueued, PhaseActing)...)
	legal = append(legal, edges(PhaseQueued, PhaseSubmitting, PhaseDeleting)...)
	legal = append(legal, edges(PhaseThrottled, PhaseQueued, PhaseDeleting)...)
	legal = append(legal, edges(PhaseSubmitting, PhaseThrottled, PhaseProvider, PhaseAwaitingLinks, PhaseActing,
		PhaseFailed, PhaseQueued, PhaseDeleting)...)
	legal = append(legal, edges(PhaseProvider, PhaseAwaitingLinks, PhaseActing, PhaseFailed, PhaseDeleting)...)
	legal = append(legal, edges(PhaseAwaitingLinks, PhaseActing, PhaseFailed, PhaseDeleting)...)
	legal = append(legal, edges(PhaseActing, PhaseComplete, PhaseFailed, PhaseDeleting)...)
	legal = append(legal, edges(PhaseComplete, PhaseFailed, PhaseDeleting)...)
	legal = append(legal, edges(PhaseFailed, PhaseQueued, PhaseActing, PhaseDeleting)...)
	legal = append(legal, edges(PhaseDeleting, PhaseAbsent)...)
	statetest.Table(t, successors, AllPhases(), legal)
}

func TestPhaseStringsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]Phase{}
	for _, p := range AllPhases() {
		s := p.String()
		if s == "" {
			t.Errorf("phase %d has no name", p)
		}
		if other, dup := seen[s]; dup {
			t.Errorf("phases %d and %d share the name %q", other, p, s)
		}
		seen[s] = p
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	type tc struct {
		name string
		st   State
		ok   bool
	}
	cases := []tc{
		{"queued", State{Phase: PhaseQueued}, true},
		{"failed with reason and time", State{Phase: PhaseFailed, Failure: Failure{Reason: "x", At: at}}, true},
		{"season acting", State{Phase: PhaseActing, PackKey: "p"}, true},
		{"failed without reason", State{Phase: PhaseFailed, Failure: Failure{At: at}}, false},
		{"failed without time", State{Phase: PhaseFailed, Failure: Failure{Reason: "x"}}, false},
		{"unknown phase", State{Phase: Phase(200)}, false},
	}
	for _, p := range []Phase{PhaseQueued, PhaseThrottled, PhaseSubmitting, PhaseProvider, PhaseAwaitingLinks} {
		cases = append(cases, tc{"season in " + p.String(), State{Phase: p, PackKey: "p"}, false})
	}
	for _, c := range cases {
		err := c.st.Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
