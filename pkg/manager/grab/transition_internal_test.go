package grab

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func at() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }

func in(p Phase) State { return State{Key: "k", Phase: p} }

func failedState() State {
	return State{Key: "k", Phase: PhaseFailed, Failure: Failure{Reason: "x", At: at(), Count: 1}}
}

func ev(k EventKind) Event { return Event{Kind: k, At: at()} }

func withOutcome(k EventKind, o Outcome) Event {
	e := ev(k)
	e.Outcome = o
	return e
}

func childName(c ChildKind) string {
	switch c {
	case ChildSubmit:
		return "submit"
	case ChildLookup:
		return "lookup"
	case ChildPoll:
		return "poll"
	case ChildLinks:
		return "links"
	case ChildAction:
		return "action"
	case ChildCleanup:
		return "cleanup"
	}
	return "?"
}

// describe names effects so a table reads as the plan's rows.
func describe(fx []Effect) []string {
	var out []string
	for _, e := range fx {
		switch e.Kind {
		case FxPersist:
			out = append(out, "persist")
		case FxRequestAdmission:
			out = append(out, "admit")
		case FxCancelAdmission:
			out = append(out, "cancel_admission")
		case FxStartChild:
			out = append(out, "start:"+childName(e.Child))
		case FxCancelChildren:
			out = append(out, "cancel_children")
		case FxReleasePlacement:
			out = append(out, "release")
		case FxFail:
			out = append(out, "fail")
		case FxRemoveRecord:
			out = append(out, "remove")
		}
	}
	return out
}

func TestTransitionRows(t *testing.T) {
	t.Parallel()
	placed := func(k EventKind, o Outcome, downloaded, linked bool) Event {
		e := withOutcome(k, o)
		e.Downloaded, e.AllLinked = downloaded, linked
		return e
	}
	observed := func(downloaded, linked, failed bool, raw string) Event {
		e := ev(EvProviderObserved)
		e.Downloaded, e.AllLinked, e.Failed, e.Raw = downloaded, linked, failed, raw
		return e
	}
	season := ev(EvSubmit)
	season.PackKey = "p"
	nzbSubmitting := in(PhaseSubmitting)
	nzbSubmitting.NZB = true

	cases := []struct {
		name    string
		from    State
		ev      Event
		want    Phase
		fx      []string
		refused bool
	}{
		{"absent submit", in(PhaseAbsent), ev(EvSubmit), PhaseQueued, []string{"persist", "admit"}, false},
		{"absent season submit", in(PhaseAbsent), season, PhaseActing, []string{"persist", "start:action"}, false},
		{"absent other", in(PhaseAbsent), ev(EvAdmitted), PhaseAbsent, nil, true},
		{"queued admission deadline", in(PhaseQueued), ev(EvAdmissionDeadline), PhaseQueued, []string{"admit"}, false},
		{
			"queued admitted",
			in(PhaseQueued),
			ev(EvAdmitted),
			PhaseSubmitting,
			[]string{"persist", "start:submit"},
			false,
		},
		{
			"queued submit result",
			in(PhaseQueued),
			withOutcome(EvSubmitResult, OutcomePlacement),
			PhaseQueued,
			nil,
			true,
		},
		{"submit throttled", in(PhaseSubmitting), withOutcome(EvSubmitResult, OutcomeThrottled), PhaseThrottled,
			[]string{"persist"}, false},
		{"parse throttled", in(PhaseSubmitting), withOutcome(EvParseResult, OutcomeThrottled), PhaseThrottled,
			[]string{"persist"}, false},
		{"throttled slot freed", in(PhaseThrottled), ev(EvSlotFreed), PhaseQueued, []string{"persist", "admit"}, false},
		{"throttled timer", in(PhaseThrottled), ev(EvThrottleTimer), PhaseQueued, []string{"persist", "admit"}, false},
		{"submit placement", in(PhaseSubmitting), placed(EvSubmitResult, OutcomePlacement, false, false),
			PhaseProvider, []string{"persist", "start:poll"}, false},
		{"parse placement", in(PhaseSubmitting), placed(EvParseResult, OutcomePlacement, false, false),
			PhaseProvider, []string{"persist", "start:poll"}, false},
		{"lookup found downloaded", in(PhaseSubmitting), placed(EvLookup, OutcomeFound, true, true),
			PhaseActing, []string{"persist", "start:action"}, false},
		{"submit permanent", in(PhaseSubmitting), withOutcome(EvSubmitResult, OutcomePermanent), PhaseFailed,
			[]string{"persist", "fail"}, false},
		{"parse permanent", in(PhaseSubmitting), withOutcome(EvParseResult, OutcomePermanent), PhaseFailed,
			[]string{"persist", "fail"}, false},
		{"lookup deadline", in(PhaseSubmitting), ev(EvLookupDeadline), PhaseFailed, []string{"persist", "fail"}, false},
		{
			"submit deadline",
			in(PhaseSubmitting),
			ev(EvSubmitDeadline),
			PhaseSubmitting,
			[]string{"start:lookup"},
			false,
		},
		{"submit unknown", in(PhaseSubmitting), withOutcome(EvSubmitResult, OutcomeUnknown), PhaseSubmitting,
			[]string{"start:lookup"}, false},
		{"lookup unknown", in(PhaseSubmitting), withOutcome(EvLookup, OutcomeUnknown), PhaseSubmitting, nil, false},
		{"lookup absent", in(PhaseSubmitting), withOutcome(EvLookup, OutcomeAbsent), PhaseQueued,
			[]string{"persist", "admit"}, false},
		{"provider poll timer", in(PhaseProvider), ev(EvPollTimer), PhaseProvider, nil, false},
		{
			"provider observed downloading",
			in(PhaseProvider),
			observed(false, false, false, ""),
			PhaseProvider,
			nil,
			false,
		},
		{"provider observed unknown", in(PhaseProvider), observed(false, false, false, "weird"), PhaseProvider,
			[]string{"persist"}, false},
		{"provider downloaded linked", in(PhaseProvider), observed(true, true, false, ""), PhaseActing,
			[]string{"persist", "start:action"}, false},
		{"provider failed", in(PhaseProvider), observed(false, false, true, ""), PhaseFailed,
			[]string{"persist", "fail"}, false},
		{"provider pending deadline", in(PhaseProvider), ev(EvPendingDeadline), PhaseFailed,
			[]string{"persist", "fail"}, false},
		{"provider placement gone", in(PhaseProvider), ev(EvPlacementGone), PhaseFailed,
			[]string{"persist", "fail"}, false},
		{"provider stalled", in(PhaseProvider), ev(EvStalledDeadline), PhaseFailed, []string{"persist", "fail"}, false},
		{
			"links ready",
			in(PhaseAwaitingLinks),
			ev(EvLinksReady),
			PhaseActing,
			[]string{"persist", "start:action"},
			false,
		},
		{"link wait expired", in(PhaseAwaitingLinks), ev(EvLinkWaitExpired), PhaseFailed,
			[]string{"persist", "fail"}, false},
		{"awaiting links provider failed", in(PhaseAwaitingLinks), observed(false, false, true, ""), PhaseFailed,
			[]string{"persist", "fail"}, false},
		{"awaiting links placement gone", in(PhaseAwaitingLinks), ev(EvPlacementGone), PhaseFailed,
			[]string{"persist", "fail"}, false},
		{"acting progress", in(PhaseActing), ev(EvActionProgress), PhaseActing, nil, false},
		{"acting child done", in(PhaseActing), ev(EvChildDone), PhaseActing, nil, false},
		{"acting seasons started", in(PhaseActing), ev(EvSeasonsStarted), PhaseActing, nil, false},
		{"provider seasons started", in(PhaseProvider), ev(EvSeasonsStarted), PhaseProvider, nil, true},
		{"acting done", in(PhaseActing), ev(EvActionDone), PhaseComplete, []string{"persist"}, false},
		{"acting failed", in(PhaseActing), ev(EvActionFailed), PhaseFailed, []string{"persist", "fail"}, false},
		{"complete dropped", in(PhaseComplete), ev(EvProviderDropped), PhaseFailed, []string{"persist", "fail"}, false},
		{"failed retry", failedState(), ev(EvSubmit), PhaseQueued, []string{"persist", "admit"}, false},
		{"complete resubmit", in(PhaseComplete), ev(EvSubmit), PhaseComplete, nil, false},
		{"active resubmit", in(PhaseProvider), ev(EvSubmit), PhaseProvider, nil, false},
		{"delete queued", in(PhaseQueued), ev(EvDelete), PhaseDeleting,
			[]string{"persist", "cancel_admission", "cancel_children", "start:cleanup"}, false},
		{"delete throttled", in(PhaseThrottled), ev(EvDelete), PhaseDeleting,
			[]string{"persist", "cancel_admission", "cancel_children", "start:cleanup"}, false},
		{"delete submitting", in(PhaseSubmitting), ev(EvDelete), PhaseDeleting,
			[]string{"persist", "cancel_children", "start:cleanup"}, false},
		{"delete complete", in(PhaseComplete), ev(EvDelete), PhaseDeleting,
			[]string{"persist", "cancel_children", "start:cleanup"}, false},
		{"delete absent", in(PhaseAbsent), ev(EvDelete), PhaseAbsent, nil, true},
		{"retention", failedState(), ev(EvFailedRetention), PhaseDeleting,
			[]string{"persist", "cancel_children", "start:cleanup"}, false},
		{"retention not failed", in(PhaseComplete), ev(EvFailedRetention), PhaseComplete, nil, true},
		{"deleting submit", in(PhaseDeleting), ev(EvSubmit), PhaseDeleting, nil, true},
		{"deleting late placement", in(PhaseDeleting), placed(EvSubmitResult, OutcomePlacement, false, false),
			PhaseDeleting, []string{"release"}, false},
		{"deleting late found", in(PhaseDeleting), withOutcome(EvLookup, OutcomeFound), PhaseDeleting,
			[]string{"release"}, false},
		{
			"deleting cleanup done",
			in(PhaseDeleting),
			ev(EvCleanupDone),
			PhaseAbsent,
			[]string{"persist", "remove"},
			false,
		},
		{"deleting other", in(PhaseDeleting), ev(EvActionDone), PhaseDeleting, nil, false},
		{"tags", in(PhaseComplete), ev(EvTagsChanged), PhaseComplete, []string{"persist"}, false},
		{"category", in(PhaseProvider), ev(EvCategoryChanged), PhaseProvider, []string{"persist"}, false},
		{"remove file", in(PhaseActing), ev(EvRemoveFile), PhaseActing, []string{"persist"}, false},
		{"acting observed", in(PhaseActing), observed(true, true, false, ""), PhaseActing, nil, true},
		{"complete observed", in(PhaseComplete), observed(false, false, true, ""), PhaseComplete, nil, true},
		{"failed observed", failedState(), observed(true, true, false, ""), PhaseFailed, nil, true},
		{"recover queued", in(PhaseQueued), ev(EvRecover), PhaseQueued, []string{"admit"}, false},
		{"recover throttled", in(PhaseThrottled), ev(EvRecover), PhaseQueued, []string{"persist", "admit"}, false},
		{"recover nzb submitting", nzbSubmitting, ev(EvRecover), PhaseQueued, []string{"persist", "admit"}, false},
		{"recover submitting", in(PhaseSubmitting), ev(EvRecover), PhaseSubmitting, []string{"start:lookup"}, false},
		{"recover provider", in(PhaseProvider), ev(EvRecover), PhaseProvider, []string{"start:poll"}, false},
		{"recover awaiting links", in(PhaseAwaitingLinks), ev(EvRecover), PhaseAwaitingLinks,
			[]string{"start:links"}, false},
		{"recover acting", in(PhaseActing), ev(EvRecover), PhaseActing, []string{"start:action"}, false},
		{"recover deleting", in(PhaseDeleting), ev(EvRecover), PhaseDeleting, []string{"start:cleanup"}, false},
		{"recover complete", in(PhaseComplete), ev(EvRecover), PhaseComplete, nil, false},
		{"recover failed", failedState(), ev(EvRecover), PhaseFailed, nil, false},
		{"recover absent", in(PhaseAbsent), ev(EvRecover), PhaseAbsent, nil, true},
	}
	for _, c := range cases {
		got, fx, err := Transition(c.from, c.ev)
		if IsRefused(err) != c.refused || (err != nil && !c.refused) {
			t.Errorf("%s: err = %v, want refused=%v", c.name, err, c.refused)
			continue
		}
		if got.Phase != c.want {
			t.Errorf("%s: phase %v, want %v", c.name, got.Phase, c.want)
		}
		if d := describe(fx); !slices.Equal(d, c.fx) {
			t.Errorf("%s: effects %v, want %v", c.name, d, c.fx)
		}
		if got.Phase != c.from.Phase && !got.PhaseSince.Equal(at()) {
			t.Errorf("%s: PhaseSince %v, want the event time", c.name, got.PhaseSince)
		}
	}
}

func TestSecondDownloadedInActingIsRefused(t *testing.T) {
	t.Parallel()
	for _, failed := range []bool{false, true} {
		e := ev(EvProviderObserved)
		e.Downloaded, e.AllLinked, e.Failed = !failed, !failed, failed
		got, fx, err := Transition(in(PhaseActing), e)
		if !IsRefused(err) || fx != nil || got.Phase != PhaseActing {
			t.Errorf("observation (failed=%v) in acting: %v, %v, %v; want refused", failed, got.Phase, fx, err)
		}
	}
}

func TestFailedRetriesToQueued(t *testing.T) {
	t.Parallel()
	from := failedState()
	got, fx, err := Transition(from, ev(EvSubmit))
	if err != nil || got.Phase != PhaseQueued {
		t.Fatalf("retry: %v, %v", got.Phase, err)
	}
	if got.Failure != from.Failure {
		t.Errorf("retry changed the failure: %+v, want %+v", got.Failure, from.Failure)
	}
	if d := describe(fx); !slices.Equal(d, []string{"persist", "admit"}) {
		t.Errorf("effects %v", d)
	}
}

func TestDownloadedWithUnlinkedFileAwaitsLinks(t *testing.T) {
	t.Parallel()
	obs := ev(EvProviderObserved)
	obs.Downloaded = true
	res := withOutcome(EvSubmitResult, OutcomePlacement)
	res.Downloaded = true
	for _, c := range []struct {
		from State
		ev   Event
	}{{in(PhaseProvider), obs}, {in(PhaseSubmitting), res}} {
		got, fx, err := Transition(c.from, c.ev)
		if err != nil || got.Phase != PhaseAwaitingLinks {
			t.Errorf("from %v: %v, %v; want awaiting_links", c.from.Phase, got.Phase, err)
		}
		if d := describe(fx); !slices.Equal(d, []string{"persist", "start:links"}) {
			t.Errorf("from %v: effects %v", c.from.Phase, d)
		}
	}
}

func TestStalledFailsWithNoExternalEffect(t *testing.T) {
	t.Parallel()
	e := ev(EvStalledDeadline)
	e.Fail = FailBadRelease
	got, fx, err := Transition(in(PhaseProvider), e)
	if err != nil || got.Phase != PhaseFailed || got.Failure.Reason != "stalled" {
		t.Fatalf("stalled: %+v, %v", got, err)
	}
	if len(fx) != 2 || fx[1].Kind != FxFail || fx[1].Fail != FailNoEffect {
		t.Errorf("effects %+v, want persist then fail(no effect)", fx)
	}
}

func TestEnteringFailedRecordsTheFailure(t *testing.T) {
	t.Parallel()
	e := ev(EvActionFailed)
	e.Reason = "disk full"
	got, _, err := Transition(in(PhaseActing), e)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Failure{Reason: "disk full", At: at(), Count: 1}); got.Failure != want {
		t.Errorf("failure %+v, want %+v", got.Failure, want)
	}
	again := failedState()
	again.Phase = PhaseComplete
	got, _, _ = Transition(again, ev(EvProviderDropped))
	if got.Failure.Count != 2 || got.Failure.Reason == "" {
		t.Errorf("second failure %+v, want count 2 and a default reason", got.Failure)
	}
}

func TestAdmittedPersistsBeforeSubmit(t *testing.T) {
	t.Parallel()
	_, fx, err := Transition(in(PhaseQueued), ev(EvAdmitted))
	if err != nil || len(fx) != 2 || fx[0].Kind != FxPersist || fx[1].Kind != FxStartChild ||
		fx[1].Child != ChildSubmit {
		t.Fatalf("admitted: %+v, %v; want persist then start(submit)", fx, err)
	}
}

func TestLateSubmitResultAfterDeleteReleasesPlacement(t *testing.T) {
	t.Parallel()
	e := withOutcome(EvSubmitResult, OutcomePlacement)
	e.Provider, e.TorrentID = "realdebrid", "T1"
	got, fx, err := Transition(in(PhaseDeleting), e)
	if err != nil || got.Phase != PhaseDeleting {
		t.Fatalf("late result: %v, %v", got.Phase, err)
	}
	want := []Effect{{Kind: FxReleasePlacement, Provider: "realdebrid", TorrentID: "T1"}}
	if !slices.Equal(fx, want) {
		t.Errorf("effects %+v, want %+v", fx, want)
	}
}

func TestSeasonChildNeverSubmits(t *testing.T) {
	t.Parallel()
	acting := State{Key: "k", Phase: PhaseActing, PackKey: "p"}
	failed := failedState()
	failed.PackKey = "p"
	events := []Event{ev(EvAdmitted), withOutcome(EvSubmitResult, OutcomePlacement), ev(EvSubmit)}
	for _, from := range []State{acting, failed} {
		for _, e := range events {
			got, _, _ := Transition(from, e)
			if got.Phase == PhaseQueued || got.Phase == PhaseSubmitting {
				t.Errorf("season child in %v given %v moved to %v", from.Phase, e.Kind, got.Phase)
			}
		}
	}
}

func TestResubmitOfAnActingSeasonStartsNothing(t *testing.T) {
	t.Parallel()
	from := State{Key: "k", Phase: PhaseActing, PackKey: "p"}
	again := ev(EvSubmit)
	again.PackKey = "p"
	got, fx, _ := Transition(from, again)
	if got != from {
		t.Errorf("state %+v, want %+v unchanged", got, from)
	}
	for _, f := range fx {
		if f.Kind == FxStartChild || f.Kind == FxPersist {
			t.Errorf("re-submit of an acting season gave effect %+v", f)
		}
	}
}

func TestNoSubmitRefusesRetry(t *testing.T) {
	t.Parallel()
	from := failedState()
	from.NoSubmit = "unmatched season"
	got, fx, err := Transition(from, ev(EvSubmit))
	var r *RefusedError
	if !errors.As(err, &r) || got != from || fx != nil {
		t.Fatalf("retry of a NoSubmit grab: %v, %v, %v; want refused", got.Phase, fx, err)
	}
	if r.Why != "unmatched season" {
		t.Errorf("why %q, want the NoSubmit reason", r.Why)
	}
	season := ev(EvSubmit)
	season.PackKey = "p"
	got, _, err = Transition(from, season)
	if err != nil || got.Phase != PhaseActing || got.PackKey != "p" {
		t.Errorf("season submit of a NoSubmit grab: %+v, %v; want acting with pack p", got, err)
	}
}

func TestRetentionNeverDeletesAtTheProvider(t *testing.T) {
	t.Parallel()
	from := failedState()
	from.Cleanup = Cleanup{DeleteFiles: true, ReleaseDelete: true}
	got, fx, err := Transition(from, ev(EvFailedRetention))
	if err != nil || got.Cleanup != (Cleanup{}) {
		t.Fatalf("retention: %+v, %v", got, err)
	}
	for _, e := range fx {
		if e.Cleanup != (Cleanup{}) {
			t.Errorf("effect %+v deletes more than the record", e)
		}
	}
}

func TestRecoverResumesTheSameCleanup(t *testing.T) {
	t.Parallel()
	del := ev(EvDelete)
	del.Cleanup = Cleanup{DeleteFiles: true, ReleaseDelete: true}
	deleting, fx, err := Transition(in(PhaseComplete), del)
	if err != nil || fx[len(fx)-1].Cleanup != del.Cleanup {
		t.Fatalf("delete: %+v, %v", fx, err)
	}
	_, fx, err = Transition(deleting, ev(EvRecover))
	want := []Effect{{Kind: FxStartChild, Child: ChildCleanup, Cleanup: del.Cleanup}}
	if err != nil || !slices.Equal(fx, want) {
		t.Errorf("recover: %+v, %v; want %+v", fx, err, want)
	}
}

func TestDeleteWithdrawsAdmission(t *testing.T) {
	t.Parallel()
	_, fx, err := Transition(in(PhaseQueued), ev(EvDelete))
	if err != nil || !slices.ContainsFunc(fx, func(e Effect) bool { return e.Kind == FxCancelAdmission }) {
		t.Errorf("delete from queued: %+v, %v; want a cancel admission", fx, err)
	}
}

func TestResubmitOfCompleteIsANoOp(t *testing.T) {
	t.Parallel()
	from := in(PhaseComplete)
	got, fx, err := Transition(from, ev(EvSubmit))
	if err != nil || fx != nil || got != from {
		t.Errorf("resubmit: %+v, %+v, %v; want no change", got, fx, err)
	}
}

func TestTagsOnAbsentRefusedOnDeletingIgnored(t *testing.T) {
	t.Parallel()
	if _, _, err := Transition(in(PhaseAbsent), ev(EvTagsChanged)); !IsRefused(err) {
		t.Errorf("tags on absent: %v, want refused", err)
	}
	got, fx, err := Transition(in(PhaseDeleting), ev(EvTagsChanged))
	if err != nil || fx != nil || got.Phase != PhaseDeleting {
		t.Errorf("tags on deleting: %v, %v, %v; want ignored", got.Phase, fx, err)
	}
}

func TestRefusalLeavesStateUnchanged(t *testing.T) {
	t.Parallel()
	from := State{Key: "k", Phase: PhaseQueued, NZB: true, UnknownRaw: "x"}
	e := withOutcome(EvLookup, OutcomeFound)
	e.PackKey = "p"
	got, fx, err := Transition(from, e)
	if !IsRefused(err) || got != from || fx != nil {
		t.Errorf("refusal: %+v, %+v, %v; want the input back", got, fx, err)
	}
}

func TestAdmittedJournalsTheSubmitAttempt(t *testing.T) {
	t.Parallel()
	got, _, err := Transition(in(PhaseQueued), ev(EvAdmitted))
	if err != nil || !got.SubmitAttempt.At.Equal(at()) {
		t.Fatalf("admitted: %+v, %v; want SubmitAttempt.At %v", got.SubmitAttempt, err, at())
	}
	res := withOutcome(EvSubmitResult, OutcomePlacement)
	res.Provider, res.TorrentID = "realdebrid", "id1"
	got, _, err = Transition(got, res)
	if err != nil || got.SubmitAttempt.Provider != "realdebrid" || !got.SubmitAttempt.At.Equal(at()) {
		t.Fatalf("submit result: %+v, %v; want provider realdebrid at %v", got.SubmitAttempt, err, at())
	}
}

func TestRecoverInSubmittingLooksUpAfterTheSubmit(t *testing.T) {
	t.Parallel()
	_, fx, err := Transition(in(PhaseSubmitting), ev(EvRecover))
	want := []Effect{{Kind: FxStartChild, Child: ChildLookup, AfterExit: ChildSubmit, HasAfter: true}}
	if err != nil || !slices.Equal(fx, want) {
		t.Errorf("recover in submitting: %+v, %v; want %+v", fx, err, want)
	}
}

func TestActionNoneDeletesACompleteRecord(t *testing.T) {
	t.Parallel()
	got, fx, err := Transition(in(PhaseComplete), ev(EvActionNone))
	want := []string{"persist", "cancel_children", "start:cleanup"}
	if err != nil || got.Phase != PhaseDeleting || got.Cleanup != (Cleanup{}) || !slices.Equal(describe(fx), want) {
		t.Fatalf("action none: %v %+v %v, %v; want deleting with a zero cleanup, %v", got.Phase, got.Cleanup,
			describe(fx), err, want)
	}
	for _, p := range AllPhases() {
		if p == PhaseComplete || p == PhaseDeleting {
			continue
		}
		if _, _, refusal := Transition(in(p), ev(EvActionNone)); !IsRefused(refusal) {
			t.Errorf("action none in %v: %v; want refused", p, refusal)
		}
	}
}
