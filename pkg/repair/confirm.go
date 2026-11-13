package repair

import (
	"context"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// MarkedBadReason is the dirty reason that queues a freshly marked entry for a recheck.
const MarkedBadReason = "marked_bad"

// Verdict is what a fresh read check of an entry's active placement decided.
type Verdict uint8

const (
	// VerdictInconclusive means the check could not tell: it never ran on a reachable provider, was cut off, or
	// met a result that proves nothing. It never marks and never clears.
	VerdictInconclusive Verdict = iota
	// VerdictBroken means a live file fails the check.
	VerdictBroken
	// VerdictHealthy means every live file passed a fresh read check on a reachable provider.
	VerdictHealthy
)

// ConfirmBroken runs a fresh read check of every live file of the active placement. An entry with no live file is
// inconclusive, never healthy.
func (r *Service) ConfirmBroken(ctx context.Context, entry *storage.Entry) Verdict {
	if !entry.IsTorrent() {
		return VerdictBroken
	}
	client := r.backend.ProviderClient(entry.ActiveProvider)
	if client == nil {
		return VerdictInconclusive
	}
	probed, passed := 0, 0
	for name, file := range entry.Files {
		if file == nil || file.Deleted {
			continue
		}
		result := r.probeTorrentFileByUnrestrict(ctx, entry, file, name,
			fileResult{name: name, infoHash: entry.InfoHash}, client, true)
		if result.broken {
			return VerdictBroken
		}
		probed++
		if result.healthy && result.fresh {
			passed++
		}
	}
	if probed == 0 || passed < probed || ctx.Err() != nil {
		return VerdictInconclusive
	}
	return VerdictHealthy
}
