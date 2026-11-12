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
	// wasHeld marks an entry the breaker held before this run probed it.
	wasHeld bool
	// inFlight marks an entry whose reacquisition is still open: its heal is under way, so it is no new evidence
	// and takes no cap slot.
	inFlight bool
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
	// inFlight counts, per provider, the entries probed whose reacquisition is still open.
	inFlight map[string]int
	// scaled says the probed counts describe the run; without them (persisted rows fixed by an operator) only
	// the count of confirmed entries is judged.
	scaled bool
	// sweep marks a batch from a sweep, whose samples may stand in for a canary that had nothing to probe.
	sweep bool
	// partial marks a sweep's batch whose probing a stop cut short: held entries it never reached are still held.
	partial bool
	usage   *runUsage
	// open are the entry IDs with a reacquisition still open (see openReacquisitions).
	open map[string]bool
}

func newProbeBatch(scaled bool, open map[string]bool) *probeBatch {
	return &probeBatch{
		entries:  make(map[string]*probedEntry),
		probed:   make(map[string]int),
		samples:  make(map[string]sweepSample),
		inFlight: make(map[string]int),
		scaled:   scaled,
		usage:    newRunUsage(),
		open:     open,
	}
}

// isInFlight reports whether any of files belongs to an entry whose reacquisition is still open.
func (b *probeBatch) isInFlight(files []storage.BrokenFile) bool {
	return slices.ContainsFunc(files, func(file storage.BrokenFile) bool { return b.open[file.InfoHash] })
}

func (b *probeBatch) noteProbed(result *probeResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, provider := range result.providers {
		b.probed[provider]++
		if b.isInFlight(result.health.BrokenFiles) {
			b.inFlight[provider]++
			continue // one being reacquired is old news: no sample of the provider
		}
		if result.wasHeld {
			continue // a held entry is suspect: no sample of the provider
		}
		sample := b.samples[provider]
		sample.probed++
		if result.health.Status == storage.HealthBroken {
			sample.broken++
		}
		b.samples[provider] = sample
	}
}

func (b *probeBatch) add(health *storage.EntryHealth, files []storage.BrokenFile, wasHeld bool) {
	if len(files) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// In flight as noteProbed decides it, on every broken file, so the scale and the evidence agree.
	b.entries[health.EntryName] = &probedEntry{
		health: health, pending: files, wasHeld: wasHeld, inFlight: b.isInFlight(health.BrokenFiles),
	}
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
	d := &healDecision{
		run: run, batch: batch, policy: policy,
		plan: make(map[Bucket]bucketOutcome), undrained: make(map[Bucket]bool),
	}
	stopped := false
	switch {
	case policy.force:
	case policy.sweep && !batch.partial:
		// Only a sweep that probed every due candidate re-probes every held entry, so only it can tell that a drain
		// is over.
		wasReleasing := r.breaker.releasing()
		defer func() {
			if !stopped {
				r.breaker.settleDrained(batch.probed, d.plan, wasReleasing, d.undrained)
			}
		}()
	case !policy.sweep:
		// A release outside a sweep closes the bucket at run end, as Admit's does, unless held rows still wait in it
		// for a sweep to drain (SM12-31).
		defer func() { r.settleOutsideSweep(drainingOf(d.plan)...) }()
	}
	names := batch.pendingNames()
	if len(names) == 0 {
		return
	}
	if !policy.force {
		d.plan = r.judgeBuckets(ctx, run.ID, batch, names)
		for bucket, out := range d.plan {
			if out.released {
				r.logger.Info().Str("bucket", bucket.String()).Msg("Repair: breaker released; draining held entries")
			}
		}
	}

	for _, name := range names {
		if ctx != nil && ctx.Err() != nil {
			stopped = true // the entries not reached are still to drain
			break
		}
		entry := batch.entries[name]
		if !entry.admitted && !r.admitEntry(d, entry) {
			continue
		}
		entry.pending = r.healBrokenEntry(ctx, run, &d.statsMu, entry.health, entry.pending)
	}
	if d.capped > 0 {
		r.logger.Info().Str("run_id", run.ID).Int("entries", d.capped).
			Msg("Repair: heal cap reached; the rest stay confirmed for a later run")
	}
}

// healDecision is what healBatch decides its entries on, and what the decisions add up to.
type healDecision struct {
	run     *storage.RepairRun
	batch   *probeBatch
	policy  healPolicy
	plan    map[Bucket]bucketOutcome
	statsMu sync.Mutex
	capped  int
	// undrained are the draining buckets that still hold entries the caps left for a later run.
	undrained map[Bucket]bool
}

// admitEntry decides an entry not yet admitted: it reports true when its heal may start, and otherwise has held
// it or left it for a later run.
func (r *Service) admitEntry(d *healDecision, entry *probedEntry) bool {
	if entry.inFlight && !d.policy.force {
		entry.pending = nil
		return false
	}
	items := r.bucketItems(entry.pending)
	if bucket, reason, held := heldBy(d.plan, items); held {
		r.holdEntry(d.run, &d.statsMu, entry, bucket, reason)
		return false
	}
	if d.policy.force {
		r.breaker.recordForced(items, d.batch.usage)
		r.releaseForced(items, d.policy.caller)
	} else if !r.breaker.tryAdmit(items, d.batch.usage) {
		d.capped++
		// Only the drain's backlog waits held; a new failure the cap left stays broken, evidence for the next sweep.
		if draining := drainingBuckets(d.plan, items); entry.wasHeld && len(draining) > 0 {
			// Saved broken it would count as new evidence next sweep and re-trip the bucket.
			r.holdEntry(d.run, &d.statsMu, entry, draining[0], holdDraining)
			for _, bucket := range draining {
				d.undrained[bucket] = true // every bucket the entry waits in stays draining
			}
			return false
		}
		entry.pending = nil
		return false
	}
	entry.admitted = true
	if entry.health.Status == storage.HealthHeld {
		entry.health.Status = storage.HealthBroken
		entry.health.HeldBucket, entry.health.HeldAt = "", time.Time{}
	}
	return true
}

// drainingBuckets names the buckets of items that are draining their held entries.
func drainingBuckets(plan map[Bucket]bucketOutcome, items []healItem) []Bucket {
	var draining []Bucket
	for _, item := range items {
		if plan[item.bucket].draining {
			draining = append(draining, item.bucket)
		}
	}
	return draining
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

// drainingOf names the buckets plan judged draining.
func drainingOf(plan map[Bucket]bucketOutcome) []Bucket {
	var draining []Bucket
	for bucket, out := range plan {
		if out.draining {
			draining = append(draining, bucket)
		}
	}
	return draining
}

// settleOutsideSweep closes the releasing buckets among keys that no persisted held row waits in. Otherwise the next
// complete sweep drains them (settleDrained). A scan that fails proves nothing, so they stay releasing (SM12-34).
// A scan that runs before a concurrent sweep saves its draining holds may close the bucket under them; those rows
// then count on a closed bucket with its canary, which holds longer and never heals more.
func (r *Service) settleOutsideSweep(keys ...Bucket) {
	if len(keys) == 0 {
		return
	}
	held := make(map[string]bool)
	err := r.storage.ForEachEntryHealth(func(h *storage.EntryHealth) error {
		if h != nil && h.Status == storage.HealthHeld {
			held[h.HeldBucket] = true
		}
		return nil
	})
	if err != nil {
		r.logger.Warn().Err(err).Msg("Repair: cannot read the held rows; released buckets stay releasing until a sweep")
		return
	}
	r.breaker.settle(held, keys...)
}

// judgeBuckets evaluates each bucket that has pending files once, on the batch as a whole.
func (r *Service) judgeBuckets(
	ctx context.Context,
	runID string,
	batch *probeBatch,
	names []string,
) map[Bucket]bucketOutcome {
	subjects, held := r.sightBuckets(batch, names)
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
		in := bucketInput{runID: runID, confirmed: confirmed, probed: probed, held: held[bucket]}
		if batch.scaled {
			in.inFlight = batch.inFlight[bucket.Provider]
		}
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

// sightBuckets returns, per bucket, the entries the batch confirms broken there and how many of them were held,
// and notes the sightings in the breaker's rolling window.
func (r *Service) sightBuckets(batch *probeBatch, names []string) (map[Bucket]map[string]struct{}, map[Bucket]int) {
	subjects := make(map[Bucket]map[string]struct{})
	held := make(map[Bucket]int)
	for _, name := range names {
		entry := batch.entries[name]
		if entry.admitted || entry.inFlight {
			continue
		}
		for _, item := range r.bucketItems(entry.pending) {
			if subjects[item.bucket] == nil {
				subjects[item.bucket] = make(map[string]struct{})
			}
			subjects[item.bucket][name] = struct{}{}
			if entry.wasHeld {
				held[item.bucket]++
			}
			// A probed run judges held entries itself; re-sighting them would refill the rolling window that heals
			// outside a sweep are judged on, and trip the bucket mid-drain.
			if !entry.wasHeld || !batch.scaled {
				r.breaker.noteConfirmed(item.bucket, name)
			}
		}
	}
	return subjects, held
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
