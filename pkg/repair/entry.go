package repair

import (
	"cmp"
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
) *probeResult {
	s := r.storage
	if c.item == nil {
		item, err := s.GetEntryItem(c.name)
		if err != nil || item == nil || len(item.Files) == 0 {
			return nil
		}
		c.item = item
	}

	h, _ := s.GetEntryHealth(c.name)
	if h == nil {
		h = &storage.EntryHealth{EntryName: c.name}
	}
	previous := h.Status
	stored := h.BrokenFiles
	markedBefore := h.Dirty && h.DirtyReason == MarkedBadReason

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

	now := r.clock()
	broken := stampObservations(r.brokenFiles(c, results), previous, stored, runID, now)
	final := rollupStatus(results)

	h.Status = final
	h.FileCount = len(names)
	h.BrokenFiles = broken
	h.BrokenCount = len(broken)
	h.Fingerprint = storage.EntryItemRepairFingerprint(c.item)
	h.LastCheckedAt = now
	h.NextCheckDueAt = h.LastCheckedAt.Add(r.recheckInterval())
	h.Dirty = false
	h.DirtyReason = ""
	r.keepMarkedBadRecheck(h, markedBefore)
	h.ActiveRunID = ""
	h.PreviousStatus = ""
	h.HeldBucket, h.HeldAt = "", time.Time{}
	if proto := firstProtocol(results); proto != "" {
		h.Protocol = proto
	}
	recordRollup(h, broken, results)
	r.saveHealth(h)
	return &probeResult{
		health:    h,
		confirmed: confirmedBroken(previous, stored, broken, runID, now, r.breakerSettings().ConfirmGap),
		providers: distinctProviders(results),
	}
}

// recordRollup stamps the times and reason that go with the rolled-up status of a finished probe.
func recordRollup(h *storage.EntryHealth, broken []storage.BrokenFile, results []fileResult) {
	switch h.Status {
	case storage.HealthHealthy:
		h.LastOKAt = h.LastCheckedAt
		h.FailureReason = ""
	case storage.HealthBroken:
		h.LastFailedAt = h.LastCheckedAt
		h.FailureReason = topReason(broken)
	case storage.HealthUnknown:
		h.FailureReason = firstFailureReason(results)
	case storage.HealthRepairing, storage.HealthUnsupported, storage.HealthStale, storage.HealthHeld:
		// rollupStatus never produces these.
	}
}

// probeResult is what one probe of an entry found.
type probeResult struct {
	health *storage.EntryHealth
	// confirmed are the broken files that passed the two-observation gate.
	confirmed []storage.BrokenFile
	// providers are the distinct providers of every file probed, the scale the breaker judges a bucket against.
	providers []string
}

func distinctProviders(results []fileResult) []string {
	var providers []string
	for _, result := range results {
		if result.provider != "" && !slices.Contains(providers, result.provider) {
			providers = append(providers, result.provider)
		}
	}
	return providers
}

func brokenFileKey(file storage.BrokenFile) string { return file.InfoHash + "\x00" + file.FileName }

// carriesObservation reports whether a stored row's broken files were a prior observation: only an entry that was
// broken (or held by the breaker) last time can be observed again.
func carriesObservation(previous storage.HealthStatus) bool {
	return previous == storage.HealthBroken || previous == storage.HealthHeld
}

// stampObservations records when and in which run each broken file was first seen. A file also stored on the
// previous broken row keeps its first observation; any other file, and a stored file from a row that predates the
// field, is first seen now.
func stampObservations(
	broken []storage.BrokenFile,
	previous storage.HealthStatus,
	stored []storage.BrokenFile,
	runID string,
	now time.Time,
) []storage.BrokenFile {
	first := make(map[string]storage.BrokenFile, len(stored))
	if carriesObservation(previous) {
		for _, file := range stored {
			if !file.FirstSeenAt.IsZero() && file.FirstSeenRunID != "" {
				first[brokenFileKey(file)] = file
			}
		}
	}
	for i := range broken {
		if seen, ok := first[brokenFileKey(broken[i])]; ok {
			broken[i].FirstSeenAt, broken[i].FirstSeenRunID = seen.FirstSeenAt, seen.FirstSeenRunID
			continue
		}
		broken[i].FirstSeenAt, broken[i].FirstSeenRunID = now, runID
	}
	return broken
}

// confirmedBroken returns the files in broken that were already stored, as a first observation from a different
// run at least gap ago, while the entry was broken or held. Anything else is a first observation.
func confirmedBroken(
	previous storage.HealthStatus,
	stored, broken []storage.BrokenFile,
	runID string,
	now time.Time,
	gap time.Duration,
) []storage.BrokenFile {
	if !carriesObservation(previous) {
		return nil
	}
	confirmable := make(map[string]bool, len(stored))
	for _, file := range stored {
		independent := !file.FirstSeenAt.IsZero() && file.FirstSeenRunID != "" && file.FirstSeenRunID != runID &&
			now.Sub(file.FirstSeenAt) >= gap
		confirmable[brokenFileKey(file)] = independent
	}
	return slices.DeleteFunc(slices.Clone(broken), func(file storage.BrokenFile) bool {
		return !confirmable[brokenFileKey(file)]
	})
}

func firstFailureReason(results []fileResult) string {
	for _, result := range results {
		if !result.healthy && result.reason != "" {
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
		result := nzb.probe(ctx, nzbProbeRequest{
			nzbID:         entry.InfoHash,
			fileName:      name,
			verifyContent: opts.VerifyContent != nil && *opts.VerifyContent,
		})
		result.provider = usenetProvider
		return result
	}
	res.provider = entry.ActiveProvider
	return r.probeTorrentFile(ctx, entry, file, name, res, opts)
}

// clearDisprovedMarks lifts the bad mark of each torrent whose live files all passed the read check.
func (r *Service) clearDisprovedMarks(ctx context.Context, results []fileResult) {
	byHash := make(map[string]map[string]fileResult)
	for _, result := range results {
		if result.infoHash == "" {
			continue
		}
		if byHash[result.infoHash] == nil {
			byHash[result.infoHash] = make(map[string]fileResult)
		}
		byHash[result.infoHash][result.name] = result
	}
	for infoHash, files := range byHash {
		entry, err := r.storage.Get(infoHash)
		if err != nil || entry == nil || !entry.Bad || !entry.IsTorrent() {
			continue
		}
		if reason := firstUnpassedReason(entry, files); reason != "" {
			r.logger.Info().Str("infohash", infoHash).Str("name", entry.Name).Str("reason", reason).
				Msg("bad mark kept")
			continue
		}
		if clearErr := r.backend.ClearBad(ctx, infoHash); clearErr != nil {
			r.logger.Warn().Err(clearErr).Str("infohash", infoHash).Msg("clearing a disproved bad mark failed")
		}
	}
}

// firstUnpassedReason names why the first live file (by name) failed the fresh read check, or "" when all passed.
func firstUnpassedReason(entry *storage.Entry, files map[string]fileResult) string {
	for _, name := range orderedFilenames(&storage.EntryItem{Files: entry.Files}) {
		result, probed := files[name]
		switch {
		case !probed:
			return "file_not_probed"
		case !result.healthy || !result.fresh:
			return cmp.Or(result.reason, "not_freshly_checked")
		}
	}
	return ""
}

// keepMarkedBadRecheck keeps a recheck that a give-up queued during this probe (not one it consumed) past its save.
func (r *Service) keepMarkedBadRecheck(h *storage.EntryHealth, markedBefore bool) {
	if markedBefore {
		return
	}
	latest, _ := r.storage.GetEntryHealth(h.EntryName)
	if latest != nil && latest.Dirty && latest.DirtyReason == MarkedBadReason {
		h.Dirty, h.DirtyReason, h.NextCheckDueAt = true, MarkedBadReason, time.Time{}
	}
}
