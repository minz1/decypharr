package types

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
)

func phaseEdges(from Phase, to ...Phase) []statetest.Edge[Phase] {
	out := make([]statetest.Edge[Phase], 0, len(to))
	for _, p := range to {
		out = append(out, statetest.Edge[Phase]{From: from, To: p})
	}
	return out
}

func TestPhaseEdges(t *testing.T) {
	t.Parallel()
	var legal []statetest.Edge[Phase]
	legal = append(legal, phaseEdges(PhaseNone, PhaseConverting, PhaseAwaitingSelection, PhaseRemoteQueued,
		PhaseDownloading, PhaseFinalizing, PhaseStalled, PhaseDownloaded, PhaseFailed)...)
	legal = append(legal, phaseEdges(PhaseConverting, PhaseAwaitingSelection, PhaseRemoteQueued, PhaseDownloading,
		PhaseFinalizing, PhaseStalled, PhaseDownloaded, PhaseFailed)...)
	legal = append(legal, phaseEdges(PhaseAwaitingSelection, PhaseRemoteQueued, PhaseDownloading, PhaseFinalizing,
		PhaseStalled, PhaseDownloaded, PhaseFailed)...)
	legal = append(legal, phaseEdges(PhaseRemoteQueued, PhaseDownloading, PhaseFinalizing, PhaseStalled,
		PhaseDownloaded, PhaseFailed)...)
	legal = append(legal, phaseEdges(PhaseDownloading, PhaseFinalizing, PhaseStalled, PhaseDownloaded, PhaseFailed)...)
	legal = append(legal, phaseEdges(PhaseFinalizing, PhaseDownloaded, PhaseFailed)...)
	legal = append(legal, phaseEdges(PhaseStalled, PhaseRemoteQueued, PhaseDownloading, PhaseFinalizing,
		PhaseDownloaded, PhaseFailed)...)
	statetest.Table(t, phaseSuccessors, AllPhases(), legal)

	refused := [][2]Phase{
		{PhaseFinalizing, PhaseDownloading},
		{PhaseDownloaded, PhaseDownloading},
		{PhaseFailed, PhaseDownloading},
		{PhaseDownloading, PhaseConverting},
	}
	for _, e := range refused {
		if LegalEdge(e[0], e[1]) {
			t.Errorf("%s -> %s is legal, want refused", e[0], e[1])
		}
	}
	if !LegalEdge(PhaseStalled, PhaseDownloaded) {
		t.Error("stalled -> downloaded is refused, want legal")
	}
}

func TestPhaseStringsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]Phase{}
	for _, p := range AllPhases() {
		s := p.String()
		if other, dup := seen[s]; dup {
			t.Errorf("phases %d and %d share the name %q", other, p, s)
		}
		seen[s] = p
		if got := PhaseNamed(s); got != p {
			t.Errorf("PhaseNamed(%q) = %v, want %v", s, got, p)
		}
	}
	if got := PhaseNamed("bogus"); got != PhaseNone {
		t.Errorf("PhaseNamed(bogus) = %v, want none", got)
	}
}

func TestTerminalPhasesHaveNoExit(t *testing.T) {
	t.Parallel()
	for _, p := range AllPhases() {
		want := p == PhaseDownloaded || p == PhaseFailed
		if p.Terminal() != want {
			t.Errorf("%s.Terminal() = %v, want %v", p, p.Terminal(), want)
		}
		if !want {
			continue
		}
		for _, q := range AllPhases() {
			if q != p && LegalEdge(p, q) {
				t.Errorf("%s -> %s is legal, want no exit", p, q)
			}
		}
	}
}

func TestNotCachedNeverFromATransientPhase(t *testing.T) {
	t.Parallel()
	for _, p := range AllPhases() {
		for _, v := range []Verdict{CachedUnknown, CachedYes} {
			for _, uncached := range []bool{false, true} {
				if NotCached(Observation{Phase: p, Cached: v}, uncached) {
					t.Errorf("NotCached(%s, %d, %v) = true", p, v, uncached)
				}
			}
		}
	}
	if NotCached(Observation{Cached: CachedNo}, true) {
		t.Error("NotCached with download_uncached = true")
	}
	if !NotCached(Observation{Cached: CachedNo}, false) {
		t.Error("NotCached of a CachedNo verdict = false")
	}
}

func TestObservationOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		t      Torrent
		phase  Phase
		raw    string
		cached Verdict
	}{
		{"downloaded", Torrent{Status: TorrentStatusDownloaded}, PhaseDownloaded, "", CachedYes},
		{"error", Torrent{Status: TorrentStatusError}, PhaseFailed, "", CachedUnknown},
		{
			"downloading 0/0 is not stalled",
			Torrent{Status: TorrentStatusDownloading},
			PhaseDownloading,
			"",
			CachedUnknown,
		},
		{"queued 0/0 is stalled", Torrent{Status: "queued"}, PhaseStalled, "", CachedUnknown},
		{"queued with seeders", Torrent{Status: "queued", Seeders: 3}, PhaseRemoteQueued, "", CachedUnknown},
		{"unknown raw with progress", Torrent{Status: "magnet_conversion", Progress: 0.1}, PhaseUnknown,
			"magnet_conversion", CachedUnknown},
		{"unknown raw 0/0 is stalled", Torrent{Status: "magnet_conversion"}, PhaseStalled, "magnet_conversion",
			CachedUnknown},
	}
	for i := range cases {
		c := &cases[i]
		o := ObservationOf(&c.t)
		if o.Phase != c.phase || o.Raw != c.raw || o.Cached != c.cached {
			t.Errorf("%s: got phase %s raw %q cached %d, want %s %q %d", c.name, o.Phase, o.Raw, o.Cached,
				c.phase, c.raw, c.cached)
		}
	}
}
