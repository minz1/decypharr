package repair

import (
	"context"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func (r *Service) probeEntry(
	ctx context.Context,
	runID string,
	c *candidate,
	heal *errorCache,
	nzb *nzbProber,
	opts RunOptions,
	autoRepair bool,
) (*storage.EntryHealth, []storage.BrokenFile) {
	s := r.storage
	if c.item == nil {
		item, err := s.GetEntryItem(c.name)
		if err != nil || item == nil || len(item.Files) == 0 {
			return nil, nil
		}
		c.item = item
	}

	h, _ := s.GetEntryHealth(c.name)
	if h == nil {
		h = &storage.EntryHealth{EntryName: c.name}
	}
	previous := h.Status
	stored := h.BrokenFiles

	h.PreviousStatus = previous
	h.Status = storage.HealthRepairing
	h.ActiveRunID = runID
	h.Protocol = ""
	r.saveHealth(h)

	names := orderedFilenames(c.item)
	results := r.probeFiles(ctx, c, names, nzb, opts)
	if autoRepair {
		r.autoHealResults(ctx, results, heal)
	}
	r.clearDisprovedMarks(ctx, results)

	broken := r.brokenFiles(c, results)
	final := rollupStatus(results)

	h.Status = final
	h.FileCount = len(names)
	h.BrokenFiles = broken
	h.BrokenCount = len(broken)
	h.Fingerprint = storage.EntryItemRepairFingerprint(c.item)
	h.LastCheckedAt = time.Now()
	h.NextCheckDueAt = h.LastCheckedAt.Add(r.recheckInterval())
	h.Dirty = false
	h.DirtyReason = ""
	h.ActiveRunID = ""
	h.PreviousStatus = ""
	if proto := firstProtocol(results); proto != "" {
		h.Protocol = proto
	}
	switch final {
	case storage.HealthHealthy:
		h.LastOKAt = h.LastCheckedAt
		h.FailureReason = ""
	case storage.HealthBroken:
		h.LastFailedAt = h.LastCheckedAt
		h.FailureReason = topReason(broken)
	case storage.HealthUnknown:
		h.FailureReason = firstDeferredReason(results)
	case storage.HealthRepairing, storage.HealthUnsupported, storage.HealthStale:
		// rollupStatus never produces these.
	}
	r.saveHealth(h)
	return h, confirmedBroken(previous, stored, broken)
}

// confirmedBroken returns the files in broken that were also in stored while the entry was broken.
func confirmedBroken(previous storage.HealthStatus, stored, broken []storage.BrokenFile) []storage.BrokenFile {
	if previous != storage.HealthBroken {
		return nil
	}
	key := func(file storage.BrokenFile) string { return file.InfoHash + "\x00" + file.FileName }
	confirmable := make(map[string]bool, len(stored))
	for _, file := range stored {
		confirmable[key(file)] = true
	}
	return slices.DeleteFunc(slices.Clone(broken), func(file storage.BrokenFile) bool {
		return !confirmable[key(file)]
	})
}

func firstDeferredReason(results []fileResult) string {
	for _, result := range results {
		if result.deferred && result.reason != "" {
			return result.reason
		}
	}
	return ""
}

func (r *Service) probeFiles(
	ctx context.Context,
	c *candidate,
	names []string,
	nzb *nzbProber,
	opts RunOptions,
) []fileResult {
	results := make([]fileResult, len(names))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(repairFilesPerEntry)
	for i, name := range names {
		g.Go(func() error {
			result := fileResult{name: name, reason: "context_cancelled"}
			if gctx.Err() == nil {
				result = r.probeFile(gctx, c, name, nzb, opts)
			}
			results[i] = result
			return nil
		})
	}
	_ = g.Wait()
	return results
}

func (r *Service) probeFile(
	ctx context.Context,
	c *candidate,
	name string,
	nzb *nzbProber,
	opts RunOptions,
) fileResult {
	res := fileResult{name: name}
	if c == nil || c.item == nil {
		res.reason = "entry_not_found"
		return res
	}
	file := c.item.Files[name]

	if file == nil || file.InfoHash == "" {
		res.reason = "missing_infohash"
		return res
	}
	res.infoHash = file.InfoHash

	entry, err := r.storage.Get(file.InfoHash)
	if err != nil || entry == nil {
		res.reason = "entry_not_found"
		return res
	}
	res.protocol = entry.Protocol
	if !repairProtocolMatches(r.effectiveProtocolScope(opts), entry.Protocol) {
		res.reason = "protocol_skipped"
		return res
	}

	if entry.IsNZB() {
		return nzb.probe(ctx, nzbProbeRequest{
			nzbID:         entry.InfoHash,
			fileName:      name,
			verifyContent: opts.VerifyContent != nil && *opts.VerifyContent,
		})
	}
	return r.probeTorrentFile(ctx, entry, file, name, res, opts)
}

// clearDisprovedMarks lifts the bad mark of each torrent whose live files all passed the read check.
func (r *Service) clearDisprovedMarks(ctx context.Context, results []fileResult) {
	healthyFiles := make(map[string]map[string]bool)
	for _, result := range results {
		if result.infoHash == "" {
			continue
		}
		if healthyFiles[result.infoHash] == nil {
			healthyFiles[result.infoHash] = make(map[string]bool)
		}
		healthyFiles[result.infoHash][result.name] = result.healthy && result.fresh
	}
	for infoHash, files := range healthyFiles {
		entry, err := r.storage.Get(infoHash)
		if err != nil || entry == nil || !entry.Bad || !entry.IsTorrent() || !allFilesHealthy(entry, files) {
			continue
		}
		if clearErr := r.backend.ClearBad(ctx, infoHash); clearErr != nil {
			r.logger.Warn().Err(clearErr).Str("infohash", infoHash).Msg("clearing a disproved bad mark failed")
		}
	}
}

func allFilesHealthy(entry *storage.Entry, healthyFiles map[string]bool) bool {
	for name, file := range entry.Files {
		if file != nil && !file.Deleted && !healthyFiles[name] {
			return false
		}
	}
	return true
}
