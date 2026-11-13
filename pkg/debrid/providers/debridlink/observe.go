package debridlink

import (
	"strconv"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func observe(entry *_torrentInfo) types.Observation {
	o := types.Observation{
		ID:       entry.ID,
		Raw:      strconv.Itoa(entry.Status),
		Progress: entry.DownloadPercent,
		Speed:    entry.DownloadSpeed,
		Seeders:  entry.PeersConnected,
	}
	switch {
	case entry.Status == seedboxDone:
		o.Phase = types.PhaseDownloaded
	case entry.ErrorID != 0:
		o.Phase = types.PhaseUnknown
	default:
		o.Phase = types.PhaseDownloading
	}
	return o
}

func torrentStatus(o types.Observation) types.TorrentStatus {
	if o.Phase == types.PhaseDownloaded {
		return types.TorrentStatusDownloaded
	}
	return types.TorrentStatusDownloading
}
