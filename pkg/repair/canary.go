package repair

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// canaryProbe probes the entries of bucket's provider that were last good most recently. Their files failing the
// way the bucket fails means the provider, not the releases, is the problem; every other failure to read them
// leaves the question open. A probe that runs out of time ends inconclusive, which only ever holds, and is logged.
func (r *Service) canaryProbe(ctx context.Context, bucket Bucket, settings config.BreakerSettings) canaryOutcome {
	names := r.canaryEntries(bucket, settings)
	if len(names) == 0 {
		return canaryNone
	}
	started := time.Now()
	outcome, progress := r.probeCanaryEntries(ctx, bucket, names)
	if outcome == canaryInconclusive && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		r.logger.Warn().Str("bucket", bucket.String()).Dur("duration", time.Since(started)).
			Int("entries", len(names)).Int("entries_probed", progress.entries).Int("files_checked", progress.files).
			Msg("Repair: canary timed out; the bucket's heals stay held")
	}
	return outcome
}

// canaryProgress is how far a canary got.
type canaryProgress struct{ entries, files int }

func (r *Service) probeCanaryEntries(
	ctx context.Context, bucket Bucket, names []string,
) (canaryOutcome, canaryProgress) {
	var progress canaryProgress
	nzb := r.nzbProber()
	outcome := canaryPass
	for _, name := range names {
		if ctx.Err() != nil {
			return canaryInconclusive, progress
		}
		progress.entries++
		item, err := r.storage.GetEntryItem(name)
		if err != nil || item == nil {
			outcome = canaryInconclusive
			continue
		}
		c := &candidate{name: name, item: item}
		for _, result := range r.probeFiles(ctx, c, orderedFilenames(item), nzb, RunOptions{}) {
			switch {
			case result.healthy:
				progress.files++
			case result.broken && canaryMatches(bucket.Class, ClassForReason(result.reason)):
				return canaryFail, progress
			default:
				outcome = canaryInconclusive
			}
		}
	}
	return outcome, progress
}

// QueueCanary is the canary of queue cleanup's blocklist guard: it probes recently good entries of the provider and
// reports whether they still read. Any provider failure counts, whatever its class. It shares the breaker's probe
// of the provider's unclassified bucket, so concurrent callers and the breaker never probe it twice at once.
func (r *Service) QueueCanary(ctx context.Context, provider string) arr.CanaryVerdict {
	switch r.breaker.probeCanary(ctx, Bucket{Provider: provider, Class: ClassOther}) {
	case canaryPass:
		return arr.CanaryPass
	case canaryFail:
		return arr.CanaryFail
	case canaryInconclusive:
		return arr.CanaryInconclusive
	case canaryAborted:
		return arr.CanaryAborted
	case canaryNone:
		return arr.CanaryNone
	}
	return arr.CanaryInconclusive
}

// canaryMatches reports whether a canary file failing with class seen is the bucket's own failure. A bucket for
// a torrent a provider dropped, or for an unclassified failure, is answered by any provider failure.
func canaryMatches(bucket, seen Class) bool {
	if bucket == ClassDropped || bucket == ClassOther {
		return true
	}
	return bucket == seen
}

// canaryEntries picks up to CanarySize healthy entries of the bucket's provider, the most recently good first:
// those good within CanaryRecent, else those good within CanaryFallback.
func (r *Service) canaryEntries(bucket Bucket, settings config.BreakerSettings) []string {
	type good struct {
		name string
		at   time.Time
	}
	if r.storage == nil {
		return nil
	}
	var healthy []good
	_ = r.storage.ForEachEntryHealth(func(h *storage.EntryHealth) error {
		if h != nil && h.Status == storage.HealthHealthy && !h.LastOKAt.IsZero() {
			healthy = append(healthy, good{name: h.EntryName, at: h.LastOKAt})
		}
		return nil
	})
	slices.SortFunc(healthy, func(a, b good) int { return b.at.Compare(a.at) })

	now := r.clock()
	for _, window := range []time.Duration{settings.CanaryRecent, settings.CanaryFallback} {
		var names []string
		for _, candidate := range healthy {
			if now.Sub(candidate.at) > window {
				break
			}
			if r.entryProvider(candidate.name) != bucket.Provider {
				continue
			}
			names = append(names, candidate.name)
			if len(names) == settings.CanarySize {
				break
			}
		}
		if len(names) > 0 {
			return names
		}
	}
	return nil
}

// entryProvider names the provider of an entry by its first live file.
func (r *Service) entryProvider(name string) string {
	item, err := r.storage.GetEntryItem(name)
	if err != nil || item == nil {
		return ""
	}
	for _, fileName := range orderedFilenames(item) {
		if file := item.Files[fileName]; file != nil && file.InfoHash != "" {
			return r.ProviderFor(file.InfoHash)
		}
	}
	return ""
}
