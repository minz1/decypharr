package repair

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/notifications"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// healPolicy says which of a batch's broken files may heal and whether the breaker applies.
type healPolicy struct {
	// confirm limits healing to files that passed the two-observation gate. Without it every broken file heals,
	// which only an operator who asked for the heal may do.
	confirm bool
	// force lets an operator heal past the breaker. The heals still count toward the caps.
	force bool
	// sweep marks the policy of a sweep rather than of a recheck or a fix.
	sweep bool
	// caller names who forced, for the log.
	caller string
}

// probedEntry is a probed entry whose broken files are waiting for a heal decision.
type probedEntry struct {
	health *storage.EntryHealth
	// pending are the files still to heal: all of them until the entry is decided, afterwards only those a
	// cancelled context cut short.
	pending []storage.BrokenFile
	// admitted marks an entry the breaker already let through, so a continuation is not judged twice.
	admitted bool
}

// probeBatch is what a run probed, held back so that every heal is decided after all the probes: the breaker
// judges a provider on the whole run, not on the entries that happened to be probed first.
type probeBatch struct {
	mu      sync.Mutex
	entries map[string]*probedEntry
	// probed counts the entries probed per provider this run.
	probed map[string]int
	// samples are, per provider, the entries probed that were not held and how many of them were broken.
	samples map[string]sweepSample
	// scaled says the probed counts describe the run; without them (persisted rows fixed by an operator) only
	// the count of confirmed entries is judged.
	scaled bool
	// sweep marks a batch from a sweep, whose samples may stand in for a canary that had nothing to probe.
	sweep bool
	usage *runUsage
}

func newProbeBatch(scaled bool) *probeBatch {
	return &probeBatch{
		entries: make(map[string]*probedEntry),
		probed:  make(map[string]int),
		samples: make(map[string]sweepSample),
		scaled:  scaled,
		usage:   newRunUsage(),
	}
}

func (b *probeBatch) noteProbed(result *probeResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, provider := range result.providers {
		b.probed[provider]++
		if result.wasHeld {
			continue // a held entry is suspect, not a sample of a working provider
		}
		sample := b.samples[provider]
		sample.probed++
		if result.health.Status == storage.HealthBroken {
			sample.broken++
		}
		b.samples[provider] = sample
	}
}

func (b *probeBatch) add(health *storage.EntryHealth, files []storage.BrokenFile) {
	if len(files) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[health.EntryName] = &probedEntry{health: health, pending: files}
}

func (b *probeBatch) pendingNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.entries))
	for name, entry := range b.entries {
		if len(entry.pending) > 0 {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// providerOf names the provider of a broken file: the one recorded when it was probed, else the entry's own.
func (r *Service) providerOf(file storage.BrokenFile) string {
	if file.Provider != "" {
		return file.Provider
	}
	if file.InfoHash != "" {
		return r.ProviderFor(file.InfoHash)
	}
	return unknownProvider
}

// ProviderFor returns the breaker provider of an entry: its active provider, "usenet" for an NZB, "unknown" when
// the entry cannot be read.
func (r *Service) ProviderFor(entryID string) string {
	if r.storage == nil || entryID == "" {
		return unknownProvider
	}
	entry, err := r.storage.Get(entryID)
	if err != nil || entry == nil {
		return unknownProvider
	}
	if entry.IsNZB() {
		return usenetProvider
	}
	if entry.ActiveProvider == "" {
		return unknownProvider
	}
	return entry.ActiveProvider
}

func (r *Service) bucketOf(file storage.BrokenFile) Bucket {
	return Bucket{Provider: r.providerOf(file), Class: ClassForReason(file.Reason)}
}

// bucketItems sums an entry's files per bucket: one heal per file.
func (r *Service) bucketItems(files []storage.BrokenFile) []healItem {
	var items []healItem
	for _, file := range files {
		bucket := r.bucketOf(file)
		if i := slices.IndexFunc(items, func(item healItem) bool { return item.bucket == bucket }); i >= 0 {
			items[i].units++
			continue
		}
		items = append(items, healItem{bucket: bucket, units: 1})
	}
	return items
}

// healBatch decides every pending heal of a batch and carries out the admitted ones. Per bucket the breaker is
// judged once on the whole batch; an entry heals only if every bucket its files fall in lets it through and the
// caps have room, is held (status held) if a bucket holds it, and otherwise stays confirmed for the next run.
func (r *Service) healBatch(ctx context.Context, run *storage.RepairRun, batch *probeBatch, policy healPolicy) {
	names := batch.pendingNames()
	if len(names) == 0 {
		return
	}
	plan := make(map[Bucket]bucketOutcome)
	if !policy.force {
		plan = r.judgeBuckets(ctx, run.ID, batch, names)
		defer r.settleReleased(plan)
	}

	var statsMu sync.Mutex
	capped := 0
	for _, name := range names {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		entry := batch.entries[name]
		if !entry.admitted {
			items := r.bucketItems(entry.pending)
			if bucket, reason, held := heldBy(plan, items); held {
				r.holdEntry(run, &statsMu, entry, bucket, reason)
				continue
			}
			if policy.force {
				r.breaker.recordForced(items, batch.usage)
				r.releaseForced(items, policy.caller)
			} else if !r.breaker.tryAdmit(items, batch.usage) {
				capped++
				entry.pending = nil
				continue
			}
			entry.admitted = true
			if entry.health.Status == storage.HealthHeld {
				entry.health.Status = storage.HealthBroken
				entry.health.HeldBucket, entry.health.HeldAt = "", time.Time{}
			}
		}
		entry.pending = r.healBrokenEntry(ctx, run, &statsMu, entry.health, entry.pending)
	}
	if capped > 0 {
		r.logger.Info().Str("run_id", run.ID).Int("entries", capped).
			Msg("Repair: heal cap reached; the rest stay confirmed for a later run")
	}
}

func heldBy(plan map[Bucket]bucketOutcome, items []healItem) (Bucket, holdReason, bool) {
	for _, item := range items {
		if out := plan[item.bucket]; out.hold != holdNone {
			return item.bucket, out.hold, true
		}
	}
	return Bucket{}, holdNone, false
}

// holdEntry parks a confirmed entry the breaker will not heal yet. It is saved as held, never as broken, and
// the next run probes it again before anything is healed.
func (r *Service) holdEntry(
	run *storage.RepairRun,
	statsMu *sync.Mutex,
	entry *probedEntry,
	bucket Bucket,
	reason holdReason,
) {
	entry.pending = nil
	health := entry.health
	health.Status = storage.HealthHeld
	health.HeldBucket = bucket.String()
	health.HeldAt = r.clock()
	r.saveHealth(health)
	statsMu.Lock()
	run.Stats.Held++
	r.saveRun(run)
	statsMu.Unlock()
	r.logger.Debug().Str("run_id", run.ID).Str("entry", health.EntryName).Str("bucket", bucket.String()).
		Str("reason", string(reason)).Msg("Repair: heal held by the breaker")
}

// judgeBuckets evaluates each bucket that has pending files once, on the batch as a whole.
func (r *Service) judgeBuckets(
	ctx context.Context,
	runID string,
	batch *probeBatch,
	names []string,
) map[Bucket]bucketOutcome {
	subjects := make(map[Bucket]map[string]struct{})
	for _, name := range names {
		entry := batch.entries[name]
		if entry.admitted {
			continue
		}
		for _, item := range r.bucketItems(entry.pending) {
			if subjects[item.bucket] == nil {
				subjects[item.bucket] = make(map[string]struct{})
			}
			subjects[item.bucket][name] = struct{}{}
			r.breaker.noteConfirmed(item.bucket, name)
		}
	}
	buckets := make([]Bucket, 0, len(subjects))
	for bucket := range subjects {
		buckets = append(buckets, bucket)
	}
	slices.SortFunc(buckets, func(a, b Bucket) int { return strings.Compare(a.String(), b.String()) })

	plan := make(map[Bucket]bucketOutcome, len(buckets))
	for _, bucket := range buckets {
		probed := -1
		if batch.scaled {
			probed = batch.probed[bucket.Provider]
		}
		confirmed := len(subjects[bucket])
		in := bucketInput{runID: runID, confirmed: confirmed, probed: probed}
		if batch.sweep {
			in.sample = batch.samples[bucket.Provider]
		}
		out := r.breaker.evaluate(ctx, bucket, in)
		plan[bucket] = out
		if out.newlyTripped {
			r.announceTrip(runID, bucket, out.hold, confirmed, probed)
		}
	}
	return plan
}

func (r *Service) settleReleased(plan map[Bucket]bucketOutcome) {
	var released []Bucket
	for bucket, out := range plan {
		if out.released {
			released = append(released, bucket)
			r.logger.Info().Str("bucket", bucket.String()).Msg("Repair: breaker released; draining held entries")
		}
	}
	r.breaker.settle(released...)
}

// announceTrip logs and notifies that a bucket's heals are held.
func (r *Service) announceTrip(runID string, bucket Bucket, reason holdReason, confirmed, probed int) {
	scale := "no probe scale"
	if probed >= 0 {
		scale = fmt.Sprintf("%d probed", probed)
	}
	r.logger.Warn().Str("run_id", runID).Str("bucket", bucket.String()).Str("reason", string(reason)).
		Int("confirmed", confirmed).Int("probed", probed).
		Msg("Repair: breaker tripped; heals for this provider and failure class are held")
	if r.notifications == nil {
		return
	}
	r.notifications.Notify(notifications.Event{
		Type:   config.EventRepairBreaker,
		Status: "warning",
		Message: fmt.Sprintf(
			"\n**Bucket**: %s\n**Why**: %s\n**Confirmed**: %d (%s)\n**Run**: %s\n"+
				"Heals are held until a later run finds it under threshold with a passing canary. "+
				"Held entries are re-probed before they heal.",
			bucket, reason, confirmed, scale, runID,
		),
	})
}
