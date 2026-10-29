package repair

import (
	"context"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// ConfirmBroken reports whether a live file of the active placement fails a fresh read check; inconclusive is false.
func (r *Service) ConfirmBroken(ctx context.Context, entry *storage.Entry) bool {
	if !entry.IsTorrent() {
		return true
	}
	client := r.backend.ProviderClient(entry.ActiveProvider)
	if client == nil {
		return true
	}
	for name, file := range entry.Files {
		if file == nil || file.Deleted {
			continue
		}
		result := r.probeTorrentFileByUnrestrict(ctx, entry, file, name,
			fileResult{name: name, infoHash: entry.InfoHash}, client, true)
		if result.broken {
			return true
		}
	}
	return false
}
