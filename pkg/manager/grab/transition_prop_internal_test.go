package grab

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/proptest"
	"github.com/sirrobot01/decypharr/internal/state"
)

const maxEvents = 40

func genEvents(seed int64) []Event {
	forward := []EventKind{
		EvSubmit, EvAdmitted, EvSubmitResult, EvProviderObserved, EvArrFailQueued, EvArrFailDone, EvSubmit,
	}
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)+1))
	events := make([]Event, 1+rng.IntN(maxEvents))
	for i := range events {
		e := Event{
			Kind:       EventKind(rng.IntN(int(evCount))),
			At:         at().Add(time.Duration(i) * time.Minute),
			Outcome:    Outcome(rng.IntN(int(outcomeCount))),
			Downloaded: rng.IntN(2) == 0,
			AllLinked:  rng.IntN(2) == 0,
			Failed:     rng.IntN(4) == 0,
			Fail:       FailEffect(rng.IntN(3)),
			Cleanup:    Cleanup{DeleteFiles: rng.IntN(2) == 0, ReleaseDelete: rng.IntN(2) == 0},
			Arr:        []string{"", "sonarr", "radarr"}[rng.IntN(3)],
		}
		e.Occurrence = at().Add(time.Duration(rng.IntN(len(events))) * time.Minute)
		e.JobCreated = at().Add(time.Duration(rng.IntN(len(events))) * time.Minute)
		if rng.IntN(3) == 0 {
			e.Kind = forward[rng.IntN(len(forward))]
		}
		if rng.IntN(3) == 0 {
			e.PackKey = "p"
		}
		if rng.IntN(4) == 0 {
			e.Raw = "weird"
		}
		events[i] = e
	}
	return events
}

// violations replays events from Absent and lists every step that breaks an invariant.
func violations(events []Event) []string {
	var out []string
	st := State{Key: "k"}
	for i, e := range events {
		next, fx, err := Transition(st, e)
		for _, v := range stepViolations(st, e, next, fx, err) {
			out = append(out, fmt.Sprintf("step %d (%v in %v): %s", i, e.Kind, st.Phase, v))
		}
		if err == nil {
			st = next
		}
	}
	return out
}

func stepViolations(st State, e Event, next State, fx []Effect, err error) []string {
	if err != nil {
		if next != st || fx != nil {
			return []string{"a refusal changed the state or emitted effects"}
		}
		return nil
	}
	var out []string
	if verr := next.Validate(); verr != nil {
		out = append(out, verr.Error())
	}
	if next.Phase != st.Phase {
		if !state.Legal(successors, st.Phase, next.Phase) {
			out = append(out, fmt.Sprintf("%v -> %v is not in the table", st.Phase, next.Phase))
		}
		if len(fx) == 0 || fx[0].Kind != FxPersist {
			out = append(out, fmt.Sprintf("%v -> %v does not persist first", st.Phase, next.Phase))
		}
		if !next.PhaseSince.Equal(e.At) {
			out = append(out, "PhaseSince is not the event time")
		}
	}
	if next.ArrFail.State != ArrFailNone && next.Phase != PhaseFailed {
		out = append(out, "an Arr fail outside Failed")
	}
	if next.PackKey != "" && (next.Phase == PhaseQueued || next.Phase == PhaseSubmitting) {
		out = append(out, "season child in "+next.Phase.String())
	}
	return out
}

func TestEveryPhaseChangeIsInTheTable(t *testing.T) {
	t.Parallel()
	proptest.CheckWhy(t, proptest.Config{Seeds: 500}, genEvents, violations, nil)
}

func TestGeneratedRunsReachEveryArrFailState(t *testing.T) {
	t.Parallel()
	reached := make(map[ArrFailState]bool)
	for seed := range int64(3000) {
		st := State{Key: "k"}
		for _, e := range genEvents(seed) {
			if next, _, err := Transition(st, e); err == nil {
				st = next
				reached[st.ArrFail.State] = true
			}
		}
	}
	for _, s := range []ArrFailState{ArrFailPending, ArrFailQueued} {
		if !reached[s] {
			t.Errorf("no generated run reached %v", s)
		}
	}
}
