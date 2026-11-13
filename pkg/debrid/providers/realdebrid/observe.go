package realdebrid

import "github.com/sirrobot01/decypharr/pkg/debrid/types"

func observe(data torrentInfo) types.Observation {
	o := types.Observation{
		ID:       data.ID,
		Raw:      data.Status,
		Progress: data.Progress,
		Speed:    data.Speed,
		Seeders:  data.Seeders,
	}
	switch data.Status {
	case statusMagnetConversion:
		o.Phase = types.PhaseConverting
	case statusWaitingFilesSelection:
		o.Phase = types.PhaseAwaitingSelection
	case statusQueued:
		o.Phase = types.PhaseRemoteQueued
	case statusDownloading, statusCompressing, statusUploading:
		o.Phase = types.PhaseDownloading
	case statusDownloaded:
		o.Phase = types.PhaseDownloaded
		if len(data.Links) == 0 {
			o.Phase = types.PhaseFinalizing
		}
	case "error", "magnet_error", "virus", "dead":
		o.Phase = types.PhaseFailed
	default:
		o.Phase = types.PhaseUnknown
	}
	return o
}
