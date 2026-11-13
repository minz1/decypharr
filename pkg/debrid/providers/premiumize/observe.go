package premiumize

import (
	"strings"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func observe(status string, linked bool) types.Observation {
	o := types.Observation{Raw: status}
	switch strings.ToLower(status) {
	case "finished", "seeding":
		o.Phase = types.PhaseDownloaded
		if !linked {
			o.Phase = types.PhaseFinalizing
		}
	case "queued", "waiting":
		o.Phase = types.PhaseRemoteQueued
		o.Stallable = true
	case "running":
		o.Phase = types.PhaseDownloading
	case "error", "deleted", "banned", "timeout":
		o.Phase = types.PhaseFailed
	default:
		o.Phase = types.PhaseUnknown
	}
	return o
}
