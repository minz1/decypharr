package manager

import "github.com/sirrobot01/decypharr/pkg/manager/grab"

const stoppedFilter = "stopped"

// QBitFilter is the set of phases a qBit torrents/info filter name lists. A nil set lists every phase: "all" and a
// name qBittorrent does not know. A name for a state no grab is in is an empty set, which lists nothing.
func QBitFilter(name string) []grab.Phase {
	switch name {
	case "downloading", "resumed", "running":
		return []grab.Phase{
			grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseProvider,
			grab.PhaseAwaitingLinks, grab.PhaseActing,
		}
	case "completed", "paused", stoppedFilter:
		return []grab.Phase{grab.PhaseComplete}
	case "active":
		return []grab.Phase{grab.PhaseProvider, grab.PhaseAwaitingLinks, grab.PhaseActing}
	case "inactive":
		return []grab.Phase{
			grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseComplete, grab.PhaseFailed,
		}
	case "errored":
		return []grab.Phase{grab.PhaseFailed}
	case "seeding", "stalled", "stalled_uploading", "stalled_downloading", "checking", "moving":
		return []grab.Phase{}
	}
	return nil
}
