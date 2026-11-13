package torbox

import (
	"strconv"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func observe(data *torboxInfo) types.Observation {
	o := types.Observation{
		ID:       strconv.Itoa(data.ID),
		Raw:      data.DownloadState,
		Progress: data.Progress * percent,
		Speed:    data.DownloadSpeed,
		Seeders:  data.Seeds,
	}
	if data.DownloadFinished {
		o.Phase = types.PhaseFinalizing
		if len(data.Files) > 0 {
			o.Phase = types.PhaseDownloaded
		}
		return o
	}
	switch torboxStatusDetail.ReplaceAllString(data.DownloadState, "") {
	case "completed", "cached", "uploading", "downloaded":
		o.Phase = types.PhaseFinalizing
	case "queued", "queuedDL":
		o.Phase = types.PhaseRemoteQueued
	case "downloading", "metaDL", "forcedDL", "checkingDL", "checkingResumeData", "allocating", "moving", "paused",
		"pausedDL", "pausedUP", "queuedUP", "checkingUP", "forcedUP", "incomplete":
		o.Phase = types.PhaseDownloading
	case "stalled", "stalledDL":
		o.Phase = types.PhaseStalled
	default:
		o.Phase = types.PhaseUnknown
	}
	return o
}
