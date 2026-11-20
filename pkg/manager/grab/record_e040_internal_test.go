package grab

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type e040document struct {
	V             int             `json:"v"`
	Phase         string          `json:"phase"`
	PhaseSince    time.Time       `json:"phase_since,omitzero"`
	PackKey       string          `json:"pack_key,omitempty"`
	NoSubmit      string          `json:"no_submit,omitempty"`
	Failure       *e040docFailure `json:"failure,omitempty"`
	Cleanup       *e040docCleanup `json:"cleanup,omitempty"`
	SubmitAttempt *e040docSubmit  `json:"submit_attempt,omitempty"`
	ProviderPhase string          `json:"provider_phase,omitempty"`
	UnknownRaw    string          `json:"unknown_raw,omitempty"`
	StalledSince  time.Time       `json:"stalled_since,omitzero"`
}

type e040docFailure struct {
	Reason string    `json:"reason"`
	Cause  string    `json:"cause,omitempty"`
	At     time.Time `json:"at"`
	Count  int       `json:"count,omitempty"`
}

type e040docCleanup struct {
	DeleteFiles   bool `json:"delete_files,omitempty"`
	ReleaseDelete bool `json:"release_delete,omitempty"`
}

type e040docSubmit struct {
	Provider string    `json:"provider"`
	At       time.Time `json:"at"`
}

func e040Load(e *storage.Entry, packOf map[string]string, inLibrary bool) (Grab, error) {
	legacyGrab := func() Grab { return Grab{State: FromLegacy(e, packOf, inLibrary), Entry: e} }
	if len(e.Grab) == 0 {
		return legacyGrab(), nil
	}
	st, err := e040FromDocument(e)
	if err != nil {
		return legacyGrab(), fmt.Errorf("grab document of %q: %w", e.InfoHash, err)
	}
	if t, ok := tupleFor(st); ok && !t.matches(e) {
		return legacyGrab(), fmt.Errorf("grab document of %q says %v, but the record says %s/%s",
			e.InfoHash, st.Phase, e.State, e.Status)
	}
	return Grab{State: st, Entry: e}, nil
}
func e040ToDocument(st State) e040document {
	d := e040document{
		V: documentVersion, Phase: st.Phase.String(), PhaseSince: st.PhaseSince, PackKey: st.PackKey,
		NoSubmit: st.NoSubmit, UnknownRaw: st.UnknownRaw,
		StalledSince: st.StalledSince,
	}
	if st.ProviderPhase != debridTypes.PhaseNone {
		d.ProviderPhase = st.ProviderPhase.String()
	}
	if st.Failure != (Failure{}) {
		d.Failure = &e040docFailure{Reason: st.Failure.Reason, Cause: st.Failure.Cause, At: st.Failure.At,
			Count: st.Failure.Count}
	}
	if st.Cleanup != (Cleanup{}) {
		d.Cleanup = &e040docCleanup{DeleteFiles: st.Cleanup.DeleteFiles, ReleaseDelete: st.Cleanup.ReleaseDelete}
	}
	if st.SubmitAttempt != (SubmitAttempt{}) {
		d.SubmitAttempt = &e040docSubmit{Provider: st.SubmitAttempt.Provider, At: st.SubmitAttempt.At}
	}
	return d
}

func e040FromDocument(e *storage.Entry) (State, error) {
	var d e040document
	if err := json.Unmarshal(e.Grab, &d); err != nil {
		return State{}, err
	}
	if d.V != documentVersion {
		return State{}, fmt.Errorf("version %d, this binary reads %d", d.V, documentVersion)
	}
	phase, ok := phaseNamed(d.Phase)
	if !ok || phase == PhaseAbsent {
		return State{}, fmt.Errorf("unknown phase %q", d.Phase)
	}
	st := State{
		Key: strings.ToLower(e.InfoHash), Phase: phase, PhaseSince: d.PhaseSince, PackKey: d.PackKey,
		NZB: e.IsNZB(), NoSubmit: d.NoSubmit, UnknownRaw: d.UnknownRaw,
		StalledSince: d.StalledSince, ProviderPhase: debridTypes.PhaseNamed(d.ProviderPhase),
	}
	if f := d.Failure; f != nil {
		st.Failure = Failure{Reason: f.Reason, Cause: f.Cause, At: f.At, Count: f.Count}
	}
	if c := d.Cleanup; c != nil {
		st.Cleanup = Cleanup{DeleteFiles: c.DeleteFiles, ReleaseDelete: c.ReleaseDelete}
	}
	if s := d.SubmitAttempt; s != nil {
		st.SubmitAttempt = SubmitAttempt{Provider: s.Provider, At: s.At}
	}
	if err := st.Validate(); err != nil {
		return State{}, err
	}
	return st, nil
}

func failedTorrentWith(a ArrFail) State {
	st, _ := stateOf(kindTorrent, PhaseFailed)
	st.ArrFail = a
	return st
}

func TestPreviousBinaryLoadsArrFail(t *testing.T) {
	t.Parallel()
	states := map[string]State{
		"pending": failedTorrentWith(ArrFail{State: ArrFailPending, Arr: "sonarr"}),
		"queued":  failedTorrentWith(ArrFail{State: ArrFailQueued, Arr: "radarr", JobID: "job-1"}),
		"skipped": failedTorrentWith(ArrFail{State: ArrFailSkipped, Arr: "sonarr"}),
	}
	del := Event{Kind: EvDelete, At: since(), Cleanup: Cleanup{DeleteFiles: true}}
	left, _, err := Transition(states["queued"], del)
	if err != nil {
		t.Fatal(err)
	}
	states["deleting after failed"] = left
	for name, st := range states {
		e := record(kindTorrent)
		if encErr := Encode(Grab{State: st, Entry: e}); encErr != nil {
			t.Fatalf("%s: Encode: %v", name, encErr)
		}
		got, loadErr := e040Load(e, nil, true)
		if loadErr != nil {
			t.Fatalf("%s: the previous binary's Load: %v", name, loadErr)
		}
		want := st
		want.ArrFail = ArrFail{}
		if !reflect.DeepEqual(got.State, want) {
			t.Errorf("%s: previous binary loaded %+v\nwant %+v", name, got.State, want)
		}
	}
}

func TestPreviousBinaryWritesNoArrFail(t *testing.T) {
	t.Parallel()
	st := failedTorrentWith(ArrFail{})
	raw, err := json.Marshal(e040ToDocument(st))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "arr_fail") {
		t.Errorf("the previous binary wrote %s", raw)
	}
	e := record(kindTorrent)
	if encErr := Encode(Grab{State: st, Entry: e}); encErr != nil {
		t.Fatal(encErr)
	}
	e.Grab = raw
	got, err := Load(e, nil, true)
	if err != nil || got.State.ArrFail != (ArrFail{}) || !reflect.DeepEqual(got.State, st) {
		t.Errorf("Load of the previous binary's document: %+v, %v", got.State, err)
	}
}
