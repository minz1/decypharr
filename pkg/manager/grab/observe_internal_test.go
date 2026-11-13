package grab

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/proptest"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func observedAs(p debridTypes.Phase, raw string, t time.Time) Event {
	return Event{Kind: EvProviderObserved, Observed: p, Raw: raw, At: t}
}

func inProvider(p debridTypes.Phase) State {
	st := in(PhaseProvider)
	st.ProviderPhase = p
	return st
}

func TestFirstObservationIsAlwaysLegal(t *testing.T) {
	t.Parallel()
	for _, p := range debridTypes.AllPhases() {
		if p == debridTypes.PhaseUnknown || p == debridTypes.PhaseNone {
			continue
		}
		for _, linked := range []bool{false, true} {
			e := observedAs(p, "", at())
			e.AllLinked = linked
			got, _, err := Transition(inProvider(debridTypes.PhaseNone), e)
			if err != nil {
				t.Errorf("%s: %v", p, err)
				continue
			}
			switch p {
			case debridTypes.PhaseDownloaded:
				want := PhaseAwaitingLinks
				if linked {
					want = PhaseActing
				}
				if got.Phase != want {
					t.Errorf("downloaded linked=%v: phase %v, want %v", linked, got.Phase, want)
				}
			case debridTypes.PhaseFailed:
				if got.Phase != PhaseFailed {
					t.Errorf("failed: phase %v", got.Phase)
				}
			default:
				if got.Phase != PhaseProvider || got.ProviderPhase != p {
					t.Errorf("%s: phase %v provider phase %v", p, got.Phase, got.ProviderPhase)
				}
			}
		}
	}
}

func TestBackwardObservationIsRefused(t *testing.T) {
	t.Parallel()
	from := inProvider(debridTypes.PhaseDownloading)
	got, fx, err := Transition(from, observedAs(debridTypes.PhaseConverting, "", at()))
	if !IsRefused(err) || got != from || fx != nil {
		t.Errorf("downloading -> converting: %+v %v %v, want refused", got, fx, err)
	}
}

func TestRepeatedPhaseIsAccepted(t *testing.T) {
	t.Parallel()
	from := inProvider(debridTypes.PhaseDownloading)
	got, fx, err := Transition(from, observedAs(debridTypes.PhaseDownloading, "", at()))
	if err != nil || fx != nil || got != from {
		t.Errorf("downloading again: %+v %v %v, want accepted with no effects", got, fx, err)
	}
}

func TestUnknownKeepsThePhase(t *testing.T) {
	t.Parallel()
	st, fx, err := Transition(inProvider(debridTypes.PhaseDownloading), observedAs(debridTypes.PhaseUnknown, "x", at()))
	if err != nil || st.ProviderPhase != debridTypes.PhaseDownloading || st.UnknownRaw != "x" ||
		!st.StalledSince.Equal(at()) || !slices.Equal(describe(fx), []string{"persist"}) {
		t.Fatalf("first unknown: %+v %v %v", st, describe(fx), err)
	}
	st, fx, err = Transition(st, observedAs(debridTypes.PhaseUnknown, "x", at().Add(time.Minute)))
	if err != nil || fx != nil || !st.StalledSince.Equal(at()) {
		t.Fatalf("second unknown: %+v %v %v", st, describe(fx), err)
	}
	st, fx, err = Transition(st, observedAs(debridTypes.PhaseDownloading, "", at().Add(2*time.Minute)))
	if err != nil || st.UnknownRaw != "" || !st.StalledSince.IsZero() ||
		!slices.Equal(describe(fx), []string{"persist"}) {
		t.Fatalf("known again: %+v %v %v", st, describe(fx), err)
	}
}

func TestStalledRunKeepsItsStart(t *testing.T) {
	t.Parallel()
	t0, t1 := at(), at().Add(time.Hour)
	st, _, err := Transition(inProvider(debridTypes.PhaseNone), observedAs(debridTypes.PhaseStalled, "", t0))
	if err != nil {
		t.Fatal(err)
	}
	st, _, err = Transition(st, observedAs(debridTypes.PhaseStalled, "", t1))
	if err != nil || !st.StalledSince.Equal(t0) {
		t.Fatalf("stalled twice: %v %v, want %v", st.StalledSince, err, t0)
	}
	st, _, err = Transition(st, observedAs(debridTypes.PhaseRemoteQueued, "", t1))
	if err != nil || !st.StalledSince.IsZero() {
		t.Fatalf("remote queued: %v %v, want zero", st.StalledSince, err)
	}

	st, _, err = Transition(inProvider(debridTypes.PhaseNone), observedAs(debridTypes.PhaseUnknown, "x", t0))
	if err != nil {
		t.Fatal(err)
	}
	st, _, err = Transition(st, observedAs(debridTypes.PhaseStalled, "", t1))
	if err != nil || !st.StalledSince.Equal(t1) || st.UnknownRaw != "" {
		t.Fatalf("unknown then stalled: %v %q %v, want %v and no raw", st.StalledSince, st.UnknownRaw, err, t1)
	}
}

func TestStalledThenDownloadedIsPlaced(t *testing.T) {
	t.Parallel()
	for _, linked := range []bool{false, true} {
		e := observedAs(debridTypes.PhaseDownloaded, "", at())
		e.AllLinked = linked
		got, _, err := Transition(inProvider(debridTypes.PhaseStalled), e)
		want := PhaseAwaitingLinks
		if linked {
			want = PhaseActing
		}
		if err != nil || got.Phase != want || got.ProviderPhase != debridTypes.PhaseNone {
			t.Errorf("linked=%v: %v %v %v, want %v", linked, got.Phase, got.ProviderPhase, err, want)
		}
	}
}

func TestResubmittedGrabStartsAtNone(t *testing.T) {
	t.Parallel()
	st := inProvider(debridTypes.PhaseDownloading)
	steps := []Event{
		withOutcome(EvActionFailed, OutcomeNone),
		ev(EvSubmit),
		ev(EvAdmitted),
		withOutcome(EvSubmitResult, OutcomePlacement),
	}
	downloaded := observedAs(debridTypes.PhaseDownloaded, "", at())
	downloaded.AllLinked = true
	st, _, err := Transition(st, downloaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range steps {
		if st, _, err = Transition(st, e); err != nil {
			t.Fatalf("%v: %v", e.Kind, err)
		}
	}
	if st.Phase != PhaseProvider || st.ProviderPhase != debridTypes.PhaseNone {
		t.Fatalf("resubmitted: %v provider phase %v, want provider/none", st.Phase, st.ProviderPhase)
	}
	if _, _, err := Transition(st, observedAs(debridTypes.PhaseConverting, "", at())); err != nil {
		t.Errorf("first observation of the new placement: %v", err)
	}
}

func genObservations(seed int64) []Event {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)+1))
	phases := debridTypes.AllPhases()
	events := make([]Event, 1+rng.IntN(maxEvents))
	for i := range events {
		e := observedAs(phases[rng.IntN(len(phases))], "", at().Add(time.Duration(i)*time.Minute))
		if rng.IntN(3) == 0 {
			e.Raw = fmt.Sprintf("raw%d", rng.IntN(3))
		}
		e.AllLinked = rng.IntN(2) == 0
		e.Fail = FailEffect(rng.IntN(3))
		events[i] = e
	}
	return events
}

func observationViolations(events []Event) []string {
	var out []string
	st := in(PhaseProvider)
	for i, e := range events {
		next, fx, err := Transition(st, e)
		for _, v := range stepViolations(st, e, next, fx, err) {
			out = append(out, fmt.Sprintf("step %d (%v in %v): %s", i, e.Observed, st.Phase, v))
		}
		leftByFailure := st.Phase == PhaseAwaitingLinks && next.Phase == PhaseFailed
		if st.Phase != PhaseProvider && err == nil && next.Phase != st.Phase && !leftByFailure {
			out = append(out, fmt.Sprintf("step %d: %v left by an observation to %v", i, st.Phase, next.Phase))
		}
		if err == nil {
			st = next
		}
	}
	return out
}

func TestRandomObservationsNeverMoveATerminalGrab(t *testing.T) {
	t.Parallel()
	proptest.CheckWhy(t, proptest.Config{Seeds: 500}, genObservations, observationViolations, nil)
}
