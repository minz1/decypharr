package types

import (
	"fmt"

	"github.com/sirrobot01/decypharr/internal/state"
)

type Phase uint8

const (
	PhaseNone Phase = iota
	PhaseUnknown
	PhaseConverting
	PhaseAwaitingSelection
	PhaseRemoteQueued
	PhaseDownloading
	PhaseFinalizing
	PhaseStalled
	PhaseDownloaded
	PhaseFailed
)

func AllPhases() []Phase {
	return []Phase{
		PhaseNone, PhaseUnknown, PhaseConverting, PhaseAwaitingSelection, PhaseRemoteQueued,
		PhaseDownloading, PhaseFinalizing, PhaseStalled, PhaseDownloaded, PhaseFailed,
	}
}

func (p Phase) String() string {
	switch p {
	case PhaseNone:
		return "none"
	case PhaseUnknown:
		return "unknown"
	case PhaseConverting:
		return "converting"
	case PhaseAwaitingSelection:
		return "awaiting_selection"
	case PhaseRemoteQueued:
		return "remote_queued"
	case PhaseDownloading:
		return "downloading"
	case PhaseFinalizing:
		return "finalizing"
	case PhaseStalled:
		return "stalled"
	case PhaseDownloaded:
		return "downloaded"
	case PhaseFailed:
		return "failed"
	}
	return fmt.Sprintf("phase(%d)", uint8(p))
}

func PhaseNamed(name string) Phase {
	for _, p := range AllPhases() {
		if p.String() == name {
			return p
		}
	}
	return PhaseNone
}

func (p Phase) Terminal() bool {
	return p == PhaseDownloaded || p == PhaseFailed
}

func phaseSuccessors(p Phase) []Phase {
	switch p {
	case PhaseNone:
		return []Phase{
			PhaseConverting, PhaseAwaitingSelection, PhaseRemoteQueued, PhaseDownloading,
			PhaseFinalizing, PhaseStalled, PhaseDownloaded, PhaseFailed,
		}
	case PhaseConverting:
		return []Phase{
			PhaseAwaitingSelection, PhaseRemoteQueued, PhaseDownloading, PhaseFinalizing,
			PhaseStalled, PhaseDownloaded, PhaseFailed,
		}
	case PhaseAwaitingSelection:
		return []Phase{PhaseRemoteQueued, PhaseDownloading, PhaseFinalizing, PhaseStalled, PhaseDownloaded, PhaseFailed}
	case PhaseRemoteQueued:
		return []Phase{PhaseDownloading, PhaseFinalizing, PhaseStalled, PhaseDownloaded, PhaseFailed}
	case PhaseDownloading:
		return []Phase{PhaseFinalizing, PhaseStalled, PhaseDownloaded, PhaseFailed}
	case PhaseFinalizing:
		return []Phase{PhaseDownloaded, PhaseFailed}
	case PhaseStalled:
		return []Phase{PhaseRemoteQueued, PhaseDownloading, PhaseFinalizing, PhaseDownloaded, PhaseFailed}
	case PhaseUnknown, PhaseDownloaded, PhaseFailed:
	}
	return nil
}

func LegalEdge(from, to Phase) bool {
	return state.Legal(phaseSuccessors, from, to)
}

type Verdict uint8

const (
	CachedUnknown Verdict = iota
	CachedYes
	CachedNo
)

type Observation struct {
	Provider string
	ID       string
	Raw      string
	Phase    Phase
	Cached   Verdict
	Progress float64
	Speed    int64
	Seeders  int
}

func NotCached(o Observation, downloadUncached bool) bool {
	return o.Cached == CachedNo && !downloadUncached
}

func ObservationOf(t *Torrent) Observation {
	o := Observation{
		Provider: t.Debrid,
		ID:       t.ID,
		Progress: t.Progress,
		Speed:    t.Speed,
		Seeders:  t.Seeders,
	}
	switch t.Status {
	case TorrentStatusDownloaded:
		o.Phase, o.Cached = PhaseDownloaded, CachedYes
	case TorrentStatusError:
		o.Phase = PhaseFailed
	case TorrentStatusDownloading:
		o.Phase = PhaseDownloading
	case TorrentStatusRemoteQueued:
		o.Phase = PhaseRemoteQueued
	default:
		o.Phase, o.Raw = PhaseUnknown, string(t.Status)
	}
	active := o.Phase == PhaseDownloading || o.Phase == PhaseDownloaded || o.Phase == PhaseFailed
	if !active && t.Seeders == 0 && t.Progress == 0 {
		o.Phase = PhaseStalled
	}
	return o
}
