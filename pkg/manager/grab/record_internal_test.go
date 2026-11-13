package grab

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type recordKind string

const (
	kindTorrent recordKind = "torrent"
	kindNZB     recordKind = "nzb"
	kindSeason  recordKind = "season"
)

func since() time.Time { return time.Date(2026, 11, 3, 4, 5, 6, 0, time.UTC) }

// record is a queue record of kind before any grab writer touched it.
func record(kind recordKind) *storage.Entry {
	e := placedAt(legacy(storage.EntryStateDownloading, debridTypes.TorrentStatusDownloading),
		debridTypes.TorrentStatusDownloading, nil)
	e.Progress = 0.4
	switch kind {
	case kindNZB:
		e.Protocol = config.ProtocolNZB
	case kindSeason:
		e.InfoHash = SeasonHash(packHash, 1)
	case kindTorrent:
	}
	return e
}

// stateOf is a valid grab state in phase p for a record of kind, or false when none is.
func stateOf(kind recordKind, p Phase) (State, bool) {
	st := State{Key: record(kind).InfoHash, Phase: p, PhaseSince: since(), NZB: kind == kindNZB}
	if kind == kindSeason {
		st.PackKey = packHash
	}
	switch p {
	case PhaseFailed:
		st.Failure = Failure{Reason: "provider failed", Cause: "magnet_error", At: since(), Count: 2}
	case PhaseSubmitting:
		st.SubmitAttempt = SubmitAttempt{Provider: "rd", At: since()}
	case PhaseDeleting:
		st.Cleanup = Cleanup{DeleteFiles: true}
	case PhaseProvider:
		st.UnknownRaw = "magnet_conversion"
	case PhaseAbsent, PhaseQueued, PhaseThrottled, PhaseAwaitingLinks, PhaseActing, PhaseComplete:
	}
	return st, p != PhaseAbsent && st.Validate() == nil
}

func TestEncodeThenLoadKeepsTheState(t *testing.T) {
	t.Parallel()
	for _, kind := range []recordKind{kindTorrent, kindNZB, kindSeason} {
		for _, p := range AllPhases() {
			if st, ok := stateOf(kind, p); ok {
				roundTrip(t, kind, st)
			}
		}
	}
}

func roundTrip(t *testing.T, kind recordKind, st State) {
	t.Helper()
	e := record(kind)
	if err := Encode(Grab{State: st, Entry: e}); err != nil {
		t.Fatalf("%s %v: Encode: %v", kind, st.Phase, err)
	}
	if len(e.Grab) == 0 {
		t.Fatalf("%s %v: no grab document written", kind, st.Phase)
	}
	got, err := Load(e, SeasonIndex([]string{packHash}), true)
	if err != nil {
		t.Fatalf("%s %v: Load: %v", kind, st.Phase, err)
	}
	if !reflect.DeepEqual(got.State, st) {
		t.Errorf("%s %v: loaded %+v\nwant %+v", kind, st.Phase, got.State, st)
	}
	if got.Entry != e {
		t.Errorf("%s %v: Load did not keep the record", kind, st.Phase)
	}
}

func TestEncodeWritesTheLegacyTuple(t *testing.T) {
	t.Parallel()
	type tuple struct {
		State      storage.TorrentState
		Status     debridTypes.TorrentStatus
		Progress   float64
		Active     string
		IsComplete bool
	}
	const (
		dl   = storage.EntryStateDownloading
		up   = storage.EntryStatePausedUP
		er   = storage.EntryStateError
		q    = debridTypes.TorrentStatusQueued
		dn   = debridTypes.TorrentStatusDownloading
		done = debridTypes.TorrentStatusDownloaded
		fail = debridTypes.TorrentStatusError
	)
	cases := []struct {
		kind recordKind
		p    Phase
		want tuple
	}{
		{kindTorrent, PhaseQueued, tuple{dl, q, 0, "", false}},
		{kindTorrent, PhaseThrottled, tuple{dl, q, 0, "", false}},
		{kindTorrent, PhaseSubmitting, tuple{dl, q, 0, "", false}},
		{kindTorrent, PhaseProvider, tuple{dl, dn, 0.4, "rd", false}},
		{kindTorrent, PhaseAwaitingLinks, tuple{dl, dn, 1, "rd", false}},
		{kindTorrent, PhaseActing, tuple{dl, done, 1, "rd", false}},
		{kindTorrent, PhaseComplete, tuple{up, done, 1, "rd", true}},
		{kindTorrent, PhaseFailed, tuple{er, fail, 0.4, "rd", false}},
		{kindTorrent, PhaseDeleting, tuple{dl, dn, 0.4, "rd", false}},
		{kindSeason, PhaseActing, tuple{"", dn, 0, "rd", false}},
		{kindSeason, PhaseComplete, tuple{up, dn, 1, "rd", true}},
		{kindSeason, PhaseFailed, tuple{er, fail, 0.4, "rd", false}},
		{kindSeason, PhaseDeleting, tuple{dl, dn, 0.4, "rd", false}},
	}
	for _, c := range cases {
		st, ok := stateOf(c.kind, c.p)
		if !ok {
			t.Fatalf("%s %v is not a valid state", c.kind, c.p)
		}
		e := record(c.kind)
		e.IsDownloading = c.p == PhaseDeleting
		if err := Encode(Grab{State: st, Entry: e}); err != nil {
			t.Fatalf("%s %v: %v", c.kind, c.p, err)
		}
		got := tuple{e.State, e.Status, e.Progress, e.ActiveProvider, e.IsComplete}
		if got != c.want {
			t.Errorf("%s %v: tuple %+v, want %+v", c.kind, c.p, got, c.want)
		}
		if c.p == PhaseFailed && (e.LastError != st.Failure.Reason || e.LastErrorTime == nil ||
			!e.LastErrorTime.Equal(st.Failure.At) || e.ErrorCount < st.Failure.Count) {
			t.Errorf("%s failed: error fields %q %v %d", c.kind, e.LastError, e.LastErrorTime, e.ErrorCount)
		}
		if c.p == PhaseComplete && e.CompletedAt == nil {
			t.Errorf("%s complete: no CompletedAt", c.kind)
		}
		if c.p != PhaseDeleting && c.p != PhaseActing && e.IsDownloading {
			t.Errorf("%s %v: IsDownloading left set", c.kind, c.p)
		}
	}
}

func TestLoadDoesNotTrustADocumentTheTupleContradicts(t *testing.T) {
	t.Parallel()
	st, _ := stateOf(kindTorrent, PhaseProvider)
	e := record(kindTorrent)
	if err := Encode(Grab{State: st, Entry: e}); err != nil {
		t.Fatal(err)
	}
	e.MarkAsError(errors.New("written by a pre-grab writer"))
	got, err := Load(e, nil, true)
	if err == nil {
		t.Fatal("no error for a stale document")
	}
	if got.State.Phase != PhaseFailed || got.State.Failure.Reason != "written by a pre-grab writer" {
		t.Fatalf("loaded %+v, want the legacy Failed", got.State)
	}
}

func TestLoadFallsBackOnAnUnreadableDocument(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"newer version": `{"v":2,"phase":"provider"}`,
		"unknown phase": `{"v":1,"phase":"teleporting"}`,
		"no phase":      `{"v":1}`,
		"not json":      `{`,
	} {
		e := record(kindTorrent)
		e.Grab = []byte(doc)
		got, err := Load(e, nil, true)
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if want := FromLegacy(e, nil, true); !reflect.DeepEqual(got.State, want) {
			t.Errorf("%s: loaded %+v, want the legacy %+v", name, got.State, want)
		}
	}
}

func TestLoadWithoutADocumentDerivesFromTheTuple(t *testing.T) {
	t.Parallel()
	e := record(kindTorrent)
	got, err := Load(e, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := FromLegacy(e, nil, true); !reflect.DeepEqual(got.State, want) {
		t.Fatalf("loaded %+v, want %+v", got.State, want)
	}
}

func TestEncodeRefusesWhatThePreviousBinaryCannotDrive(t *testing.T) {
	t.Parallel()
	provider, _ := stateOf(kindTorrent, PhaseProvider)
	noActive := record(kindTorrent)
	noActive.ActiveProvider = ""
	cases := map[string]Grab{
		"absent":          {State: State{Key: packHash}, Entry: record(kindTorrent)},
		"invalid":         {State: State{Key: packHash, Phase: PhaseFailed}, Entry: record(kindTorrent)},
		"no record":       {State: provider},
		"key mismatch":    {State: State{Key: otherHash, Phase: PhaseQueued}, Entry: record(kindTorrent)},
		"provider, no rd": {State: provider, Entry: noActive},
		"acting, no rd":   {State: State{Key: packHash, Phase: PhaseActing}, Entry: noActive},
		"complete, no rd": {State: State{Key: packHash, Phase: PhaseComplete}, Entry: noActive},
		"awaiting, no rd": {State: State{Key: packHash, Phase: PhaseAwaitingLinks}, Entry: noActive},
	}
	for name, g := range cases {
		var before []byte
		if g.Entry != nil {
			before = g.Entry.Grab
		}
		if err := Encode(g); err == nil {
			t.Errorf("%s: Encode accepted it", name)
		}
		if g.Entry != nil && !reflect.DeepEqual(g.Entry.Grab, before) {
			t.Errorf("%s: a refused Encode wrote a document", name)
		}
	}
}
