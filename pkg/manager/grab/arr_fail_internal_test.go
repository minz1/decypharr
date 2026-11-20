package grab

import (
	"reflect"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func TestArrFailTable(t *testing.T) {
	t.Parallel()
	all := []ArrFailState{ArrFailNone, ArrFailPending, ArrFailQueued, ArrFailSkipped}
	legal := []statetest.Edge[ArrFailState]{
		{From: ArrFailNone, To: ArrFailPending},
		{From: ArrFailPending, To: ArrFailQueued},
		{From: ArrFailPending, To: ArrFailNone},
		{From: ArrFailQueued, To: ArrFailNone},
		{From: ArrFailSkipped, To: ArrFailNone},
	}
	statetest.Table(t, arrFailSuccessors, all, legal)
	for _, from := range all {
		for _, to := range all {
			_, err := ArrFail{State: from, Arr: "sonarr"}.to(to)
			want := false
			for _, e := range legal {
				want = want || (e.From == from && e.To == to)
			}
			if (err == nil) != want {
				t.Errorf("to(%v -> %v) err = %v, want legal=%v", from, to, err, want)
			}
		}
	}
	cleared, err := ArrFail{State: ArrFailQueued, Arr: "sonarr", JobID: "j"}.to(ArrFailNone)
	if err != nil || cleared != (ArrFail{}) {
		t.Errorf("to(none) = %+v, %v, want the zero value", cleared, err)
	}
}

func TestArrFailStrings(t *testing.T) {
	t.Parallel()
	for _, s := range []ArrFailState{ArrFailNone, ArrFailPending, ArrFailQueued, ArrFailSkipped} {
		if got := arrFailNamed(s.String()); got != s {
			t.Errorf("arrFailNamed(%q) = %v, want %v", s.String(), got, s)
		}
	}
	if got := arrFailNamed("told_later"); got != ArrFailSkipped {
		t.Errorf("an unknown name decodes to %v, want skipped", got)
	}
}

func providerFailed(arr string, effect FailEffect) (State, Event) {
	st := State{Key: "k", Phase: PhaseProvider}
	e := ev(EvProviderObserved)
	e.Failed, e.Fail, e.Arr = true, effect, arr
	return st, e
}

func TestFailSetsPendingOnlyForArrBadRelease(t *testing.T) {
	t.Parallel()
	type tc struct {
		name   string
		mutate func(*State, *Event)
		want   ArrFail
	}
	cases := []tc{
		{"bad release with an Arr", func(*State, *Event) {}, ArrFail{State: ArrFailPending, Arr: "sonarr"}},
		{"fail entry", func(_ *State, e *Event) { e.Fail = FailEntry }, ArrFail{}},
		{"no effect", func(_ *State, e *Event) { e.Fail = FailNoEffect }, ArrFail{}},
		{"nzb", func(s *State, _ *Event) { s.NZB = true }, ArrFail{}},
		{"no Arr", func(_ *State, e *Event) { e.Arr = "" }, ArrFail{}},
	}
	for _, c := range cases {
		st, e := providerFailed("sonarr", FailBadRelease)
		c.mutate(&st, &e)
		next, _, err := Transition(st, e)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if next.Phase != PhaseFailed || next.ArrFail != c.want {
			t.Errorf("%s: %v %+v, want Failed %+v", c.name, next.Phase, next.ArrFail, c.want)
		}
	}
	pack := State{Key: "k", Phase: PhaseActing, PackKey: "p"}
	e := ev(EvActionFailed)
	e.Fail, e.Arr = FailBadRelease, "sonarr"
	next, _, err := Transition(pack, e)
	if err != nil || next.ArrFail != (ArrFail{}) {
		t.Errorf("season child: %+v, %v, want none", next.ArrFail, err)
	}
}

func TestFailEffectDecidesNotTheEventField(t *testing.T) {
	t.Parallel()
	st := State{Key: "k", Phase: PhaseProvider}
	e := ev(EvStalledDeadline)
	e.Fail, e.Arr = FailBadRelease, "sonarr"
	next, _, err := Transition(st, e)
	if err != nil || next.Phase != PhaseFailed || next.ArrFail != (ArrFail{}) {
		t.Errorf("a stall: %v %+v %v, want Failed with no intent", next.Phase, next.ArrFail, err)
	}
}

func arrFailed(s ArrFailState) State {
	st := failedState()
	if s != ArrFailNone {
		st.ArrFail = ArrFail{State: s, Arr: "sonarr"}
	}
	if s == ArrFailQueued {
		st.ArrFail.JobID = "job-1"
	}
	return st
}

func TestLeavingFailedClearsArrFail(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		from  ArrFailState
		event EventKind
		phase Phase
	}{
		"resubmit": {ArrFailPending, EvSubmit, PhaseQueued},
		"delete":   {ArrFailQueued, EvDelete, PhaseDeleting},
	} {
		from := arrFailed(c.from)
		from.Key = record(kindTorrent).InfoHash
		next, _, err := Transition(from, ev(c.event))
		if err != nil || next.Phase != c.phase || next.ArrFail != (ArrFail{}) {
			t.Errorf("%s: %v %+v %v, want %v with no intent", name, next.Phase, next.ArrFail, err, c.phase)
		}
		if encErr := Encode(Grab{State: next, Entry: record(kindTorrent)}); encErr != nil {
			t.Errorf("%s: Encode: %v", name, encErr)
		}
	}
}

func TestArrFailQueuedRows(t *testing.T) {
	t.Parallel()
	other := failedState().Failure.At.In(time.FixedZone("x", 3600))
	e := ev(EvArrFailQueued)
	e.Occurrence, e.JobID = other, "job-9"
	next, fx, err := Transition(arrFailed(ArrFailPending), e)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ArrFail{State: ArrFailQueued, Arr: "sonarr", JobID: "job-9"}); next.ArrFail != want {
		t.Errorf("ArrFail = %+v, want %+v", next.ArrFail, want)
	}
	if next.Phase != PhaseFailed || !reflect.DeepEqual(describe(fx), []string{"persist"}) {
		t.Errorf("phase %v effects %v, want Failed and [persist]", next.Phase, describe(fx))
	}
	late := e
	late.Occurrence = failedState().Failure.At.Add(time.Nanosecond)
	if _, _, lateErr := Transition(arrFailed(ArrFailPending), late); !IsRefused(lateErr) {
		t.Errorf("another occurrence: %v, want a refusal", lateErr)
	}
	for name, st := range map[string]State{
		"failed queued": arrFailed(ArrFailQueued),
		"failed none":   failedState(),
		"complete":      in(PhaseComplete),
	} {
		if _, _, refErr := Transition(st, e); !IsRefused(refErr) {
			t.Errorf("%s: %v, want a refusal", name, refErr)
		}
	}
}

func TestArrFailDoneRows(t *testing.T) {
	t.Parallel()
	jobAt := at().Add(time.Hour)
	done := func(arr string) Event {
		e := ev(EvArrFailDone)
		e.Arr, e.JobCreated = arr, jobAt
		return e
	}
	for _, s := range []ArrFailState{ArrFailPending, ArrFailQueued, ArrFailNone} {
		next, fx, err := Transition(arrFailed(s), done("sonarr"))
		if err != nil || next.Phase != PhaseDeleting || next.ArrFail != (ArrFail{}) ||
			next.Cleanup != (Cleanup{DeleteFiles: true}) {
			t.Errorf("failed %v: %v %+v %+v %v, want Deleting cleaning files",
				s, next.Phase, next.ArrFail, next.Cleanup, err)
		}
		if len(fx) == 0 || fx[0].Kind != FxPersist {
			t.Errorf("failed %v: effects %v, want persist first", s, describe(fx))
		}
	}
	if _, _, err := Transition(arrFailed(ArrFailQueued), done("radarr")); !IsRefused(err) {
		t.Errorf("another Arr: %v, want a refusal", err)
	}
	unfrozen := failedState()
	if next, _, err := Transition(unfrozen, done("radarr")); err != nil || next.Phase != PhaseDeleting {
		t.Errorf("no frozen Arr: %v %v, want Deleting", next.Phase, err)
	}
	complete := in(PhaseComplete)
	for name, since := range map[string]time.Time{"equal": jobAt, "before": jobAt.Add(-time.Second)} {
		complete.PhaseSince = since
		if next, _, err := Transition(complete, done("sonarr")); err != nil || next.Phase != PhaseDeleting {
			t.Errorf("complete %s: %v %v, want Deleting", name, next.Phase, err)
		}
	}
	complete.PhaseSince = jobAt.Add(time.Nanosecond)
	if _, _, err := Transition(complete, done("sonarr")); !IsRefused(err) {
		t.Errorf("complete after the job: %v, want a refusal", err)
	}
	for _, p := range []Phase{PhaseProvider, PhaseActing, PhaseQueued} {
		if _, _, err := Transition(in(p), done("sonarr")); !IsRefused(err) {
			t.Errorf("%v: %v, want a refusal", p, err)
		}
	}
	deleting := State{Key: "k", Phase: PhaseDeleting, Cleanup: Cleanup{DeleteFiles: true}}
	next, fx, err := Transition(deleting, done("sonarr"))
	if err != nil || next != deleting || fx != nil {
		t.Errorf("deleting: %+v %v %v, want unchanged and no error", next, fx, err)
	}
}

func TestValidateArrFail(t *testing.T) {
	t.Parallel()
	good := arrFailed(ArrFailPending)
	type tc struct {
		name   string
		mutate func(*State)
		ok     bool
	}
	cases := []tc{
		{"pending", func(*State) {}, true},
		{"queued with a job", func(s *State) { s.ArrFail = arrFailed(ArrFailQueued).ArrFail }, true},
		{"skipped", func(s *State) { s.ArrFail.State = ArrFailSkipped }, true},
		{"outside failed", func(s *State) { s.Phase = PhaseComplete }, false},
		{"pack key", func(s *State) { s.PackKey = "p" }, false},
		{"nzb", func(s *State) { s.NZB = true }, false},
		{"no arr", func(s *State) { s.ArrFail.Arr = "" }, false},
		{"job outside queued", func(s *State) { s.ArrFail.JobID = "j" }, false},
		{"data without a state", func(s *State) { s.ArrFail.State = ArrFailNone }, false},
	}
	for _, c := range cases {
		st := good
		c.mutate(&st)
		if err := st.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
