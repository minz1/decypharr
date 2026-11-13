package grab

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// documentVersion is the grab document version this binary writes and reads.
const documentVersion = 1

// Grab is one queue record as the grab's owner sees it: the grab's state, and the record that carries the
// request data and the legacy fields an older binary reads.
type Grab struct {
	State State
	Entry *storage.Entry
}

// document is the grab state as persisted in the queue record (storage.Entry.Grab). Later fields are new
// omitempty keys; a reader ignores keys it does not know. The phase is stored by name so its number can change.
type document struct {
	V             int         `json:"v"`
	Phase         string      `json:"phase"`
	PhaseSince    time.Time   `json:"phase_since,omitzero"`
	PackKey       string      `json:"pack_key,omitempty"`
	NoSubmit      string      `json:"no_submit,omitempty"`
	Failure       *docFailure `json:"failure,omitempty"`
	Cleanup       *docCleanup `json:"cleanup,omitempty"`
	SubmitAttempt *docSubmit  `json:"submit_attempt,omitempty"`
	ProviderPhase string      `json:"provider_phase,omitempty"`
	UnknownRaw    string      `json:"unknown_raw,omitempty"`
	StalledSince  time.Time   `json:"stalled_since,omitzero"`
}

type docFailure struct {
	Reason string    `json:"reason"`
	Cause  string    `json:"cause,omitempty"`
	At     time.Time `json:"at"`
	Count  int       `json:"count,omitempty"`
}

type docCleanup struct {
	DeleteFiles   bool `json:"delete_files,omitempty"`
	ReleaseDelete bool `json:"release_delete,omitempty"`
}

type docSubmit struct {
	Provider string    `json:"provider"`
	At       time.Time `json:"at"`
}

// legacyTuple is the State/Status/IsComplete a record in a phase carries for an older binary.
type legacyTuple struct {
	state    storage.TorrentState
	status   storage.EntryStatus // empty: any (a season child keeps the status it was created with)
	complete bool
}

// tupleFor is the tuple Encode writes for st; false for Deleting, which keeps the tuple it had.
func tupleFor(st State) (legacyTuple, bool) {
	if st.season() {
		switch st.Phase {
		case PhaseActing:
			return legacyTuple{state: "", status: storage.EntryStatusDownloading}, true
		case PhaseComplete:
			return legacyTuple{state: storage.EntryStatePausedUP, complete: true}, true
		case PhaseFailed:
			return legacyTuple{state: storage.EntryStateError, status: storage.EntryStatusError}, true
		case PhaseAbsent, PhaseQueued, PhaseThrottled, PhaseSubmitting, PhaseProvider, PhaseAwaitingLinks,
			PhaseDeleting:
			return legacyTuple{}, false
		}
	}
	switch st.Phase {
	case PhaseQueued, PhaseThrottled, PhaseSubmitting:
		return legacyTuple{state: storage.EntryStateDownloading, status: storage.EntryStatusQueued}, true
	case PhaseProvider, PhaseAwaitingLinks:
		return legacyTuple{state: storage.EntryStateDownloading, status: storage.EntryStatusDownloading}, true
	case PhaseActing:
		return legacyTuple{state: storage.EntryStateDownloading, status: storage.EntryStatusDownloaded}, true
	case PhaseComplete:
		return legacyTuple{
			state: storage.EntryStatePausedUP, status: storage.EntryStatusDownloaded, complete: true,
		}, true
	case PhaseFailed:
		return legacyTuple{state: storage.EntryStateError, status: storage.EntryStatusError}, true
	case PhaseAbsent, PhaseDeleting:
	}
	return legacyTuple{}, false
}

// matches reports whether e carries t.
func (t legacyTuple) matches(e *storage.Entry) bool {
	return e.State == t.state && (t.status == "" || e.Status == t.status) && e.IsComplete == t.complete
}

// needsActive reports the phases whose record an older binary only drives with an active placement.
func needsActive(st State, e *storage.Entry) bool {
	if st.season() || e.IsNZB() {
		return false
	}
	switch st.Phase {
	case PhaseProvider, PhaseAwaitingLinks, PhaseActing, PhaseComplete:
		return true
	case PhaseAbsent, PhaseQueued, PhaseThrottled, PhaseSubmitting, PhaseFailed, PhaseDeleting:
	}
	return false
}

// Encode writes g's state into g.Entry: the legacy fields an older binary reads (so it acts on the record as on
// one it wrote itself) and the grab document. It refuses, writing nothing, a state that is absent or invalid, and
// one an older binary would strand.
func Encode(g Grab) error {
	st, e := g.State, g.Entry
	switch {
	case e == nil:
		return errors.New("grab without a record")
	case st.Phase == PhaseAbsent:
		return errors.New("an absent grab has no record")
	case st.Key != strings.ToLower(e.InfoHash):
		return fmt.Errorf("grab %q in the record of %q", st.Key, e.InfoHash)
	case needsActive(st, e) && e.ActiveProvider == "":
		return fmt.Errorf("grab %q %v without an active placement", st.Key, st.Phase)
	}
	if err := st.Validate(); err != nil {
		return err
	}
	doc, err := json.Marshal(toDocument(st))
	if err != nil {
		return fmt.Errorf("encode grab %q: %w", st.Key, err)
	}
	writeTuple(st, e)
	e.Grab = doc
	return nil
}

// writeTuple sets the legacy fields of e for st (spec §3.1). Season children get p1's own tuples.
func writeTuple(st State, e *storage.Entry) {
	t, ok := tupleFor(st)
	if !ok {
		return
	}
	if st.Phase != PhaseActing {
		e.IsDownloading = false
	}
	switch st.Phase {
	case PhaseQueued, PhaseThrottled, PhaseSubmitting:
		e.Progress = 0
		e.ClearActive()
	case PhaseAwaitingLinks:
		e.Progress = 1
	case PhaseActing:
		if !e.IsDownloading || e.Progress == 0 {
			e.Progress = 1
		}
		if st.season() {
			e.Progress = 0
		}
	case PhaseComplete:
		if !e.IsComplete || e.State != storage.EntryStatePausedUP {
			e.MarkAsCompleted(e.ContentPath)
		}
	case PhaseFailed:
		if e.State != storage.EntryStateError {
			e.MarkAsError(errors.New(st.Failure.Reason))
		}
		at := st.Failure.At
		e.LastError, e.LastErrorTime, e.ErrorCount = st.Failure.Reason, &at, max(e.ErrorCount, st.Failure.Count)
	case PhaseAbsent, PhaseProvider, PhaseDeleting:
	}
	e.State, e.IsComplete = t.state, t.complete
	if t.status != "" {
		e.Status = t.status
	}
}

// Load reads the grab state of a queue record. A record with a readable grab document whose legacy fields agree
// with it loads the document; any other record is derived from its legacy fields (FromLegacy), and a non-nil
// error says why a document present was not used. The returned Grab is valid either way.
func Load(e *storage.Entry, packOf map[string]string, inLibrary bool) (Grab, error) {
	legacyGrab := func() Grab { return Grab{State: FromLegacy(e, packOf, inLibrary), Entry: e} }
	if len(e.Grab) == 0 {
		return legacyGrab(), nil
	}
	st, err := fromDocument(e)
	if err != nil {
		return legacyGrab(), fmt.Errorf("grab document of %q: %w", e.InfoHash, err)
	}
	if t, ok := tupleFor(st); ok && !t.matches(e) {
		return legacyGrab(), fmt.Errorf("grab document of %q says %v, but the record says %s/%s",
			e.InfoHash, st.Phase, e.State, e.Status)
	}
	return Grab{State: st, Entry: e}, nil
}

func toDocument(st State) document {
	d := document{
		V: documentVersion, Phase: st.Phase.String(), PhaseSince: st.PhaseSince, PackKey: st.PackKey,
		NoSubmit: st.NoSubmit, UnknownRaw: st.UnknownRaw,
		StalledSince: st.StalledSince,
	}
	if st.ProviderPhase != debridTypes.PhaseNone {
		d.ProviderPhase = st.ProviderPhase.String()
	}
	if st.Failure != (Failure{}) {
		d.Failure = &docFailure{Reason: st.Failure.Reason, Cause: st.Failure.Cause, At: st.Failure.At,
			Count: st.Failure.Count}
	}
	if st.Cleanup != (Cleanup{}) {
		d.Cleanup = &docCleanup{DeleteFiles: st.Cleanup.DeleteFiles, ReleaseDelete: st.Cleanup.ReleaseDelete}
	}
	if st.SubmitAttempt != (SubmitAttempt{}) {
		d.SubmitAttempt = &docSubmit{Provider: st.SubmitAttempt.Provider, At: st.SubmitAttempt.At}
	}
	return d
}

func fromDocument(e *storage.Entry) (State, error) {
	var d document
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

// phaseNamed is the phase whose String is name.
func phaseNamed(name string) (Phase, bool) {
	for _, p := range AllPhases() {
		if p.String() == name {
			return p, true
		}
	}
	return PhaseAbsent, false
}
