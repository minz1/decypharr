package repair

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/flight"
)

// Class is the kind of failure a heal answers. With the provider it names a breaker bucket, so an outage of one
// provider's hosters is judged apart from missing usenet articles or a provider that dropped a torrent.
type Class string

const (
	// ClassHosterLink is a hoster that is down or a download link that cannot be generated.
	ClassHosterLink Class = "hoster_link"
	// ClassUsenetSegment is a usenet article the servers no longer have.
	ClassUsenetSegment Class = "usenet_segment"
	// ClassUsenetManifest is an NZB whose manifest or content is missing, invalid or corrupt.
	ClassUsenetManifest Class = "usenet_manifest"
	// ClassDropped is a torrent its provider no longer lists.
	ClassDropped Class = "dropped"
	// ClassOther is every other failure that leads to an Arr file being deleted.
	ClassOther Class = "other"
)

// usenetProvider is the provider name of every NZB entry.
const usenetProvider = "usenet"

// unknownProvider names the provider of an entry that cannot be read.
const unknownProvider = "unknown"

// ClassForReason returns the class of a probe failure reason.
func ClassForReason(reason string) Class {
	switch reason {
	case reasonHosterUnavailable, reasonEmptyDownloadLink:
		return ClassHosterLink
	case reasonUsenetSegmentMissing:
		return ClassUsenetSegment
	case reasonUsenetCorruptContent, "usenet_manifest_missing", "usenet_manifest_invalid":
		return ClassUsenetManifest
	case "torrent_not_found", "placement_not_found":
		// The provider no longer has the torrent: the same event the manager files under dropped.
		return ClassDropped
	default:
		return ClassOther
	}
}

// Bucket is the unit the breaker judges: one provider and one class of failure.
type Bucket struct {
	Provider string
	Class    Class
}

func (b Bucket) String() string { return b.Provider + "/" + string(b.Class) }

// breakerState is the state of one bucket.
//
//	closed    heals pass, within the caps
//	tripped   heals are held; entered when a run or the rolling window finds too many confirmed failures
//	releasing a later run found the bucket under threshold with a passing canary; its held entries are
//	          re-probed and drained through the caps, over as many runs as the caps need, then the bucket closes
type breakerState string

const (
	breakerClosed    breakerState = "closed"
	breakerTripped   breakerState = "tripped"
	breakerReleasing breakerState = "releasing"
)

// successors lists every state a bucket may move to from s; it is the whole transition table.
func (s breakerState) successors() []breakerState {
	switch s {
	case breakerClosed:
		return []breakerState{breakerTripped}
	case breakerTripped:
		// tripped -> tripped is a fresh over-threshold observation: it restarts the release clock.
		return []breakerState{breakerTripped, breakerReleasing}
	case breakerReleasing:
		return []breakerState{breakerTripped, breakerClosed}
	}
	return nil
}

var errBreakerEdge = errors.New("illegal breaker transition")

// bucketBreaker is the state of one bucket and the evidence it was judged on.
type bucketBreaker struct {
	state breakerState
	// since is when the bucket entered its state; a refreshed trip counts as entering again.
	since time.Time
	// trippedRun is the sweep that last tripped the bucket, "" when no sweep did; only a later run releases it.
	trippedRun string
	reason     holdReason
	// confirmed holds each subject confirmed broken, for the rolling window.
	confirmed map[string]*confirmation
}

// transition is the only way a bucket changes state.
func (b *bucketBreaker) transition(to breakerState, at time.Time, runID string, reason holdReason) error {
	if !slices.Contains(b.state.successors(), to) {
		return fmt.Errorf("%w: %s -> %s", errBreakerEdge, b.state, to)
	}
	b.state, b.since = to, at
	if to == breakerTripped {
		b.trippedRun, b.reason = runID, reason
	}
	return nil
}

type holdReason string

const (
	holdNone               holdReason = ""
	holdTripped            holdReason = "tripped"
	holdCanaryFailed       holdReason = "canary_failed"
	holdCanaryInconclusive holdReason = "canary_inconclusive"
	// holdNoCanary holds a tripped bucket that has nothing recently good to probe: only a later sweep that samples
	// the provider, or an operator force, releases it.
	holdNoCanary holdReason = "no_canary"
	// holdDraining holds an entry of a releasing bucket that the caps left for the next run of the drain.
	holdDraining holdReason = "draining"
)

// hint says what a bucket held for this reason is waiting for.
func (r holdReason) hint() string {
	if r == holdNoCanary {
		return "waiting for a sweep or force"
	}
	return ""
}

type canaryOutcome int

const (
	// canaryNone is no signal: no recently good entry exists to probe.
	canaryNone canaryOutcome = iota
	canaryPass
	canaryFail
	// canaryInconclusive is a canary that neither passed nor showed this bucket's failure.
	canaryInconclusive
	// canaryAborted is a probe cut off because the service stopped. It says nothing about the provider, so it holds
	// the evaluation that asked but is never cached and never trips a bucket.
	canaryAborted
)

type canaryVerdict struct {
	outcome canaryOutcome
	at      time.Time
}

// canaryFunc probes recently good entries of the bucket's provider.
type canaryFunc func(ctx context.Context, bucket Bucket, settings config.BreakerSettings) canaryOutcome

type healRecord struct {
	at     time.Time
	bucket Bucket
	units  int
}

type healItem struct {
	bucket Bucket
	units  int
}

// runUsage counts what one run (a sweep, or a batch of operator work) has already admitted.
type runUsage struct {
	total     int
	perBucket map[Bucket]int
}

func newRunUsage() *runUsage { return &runUsage{perBucket: make(map[Bucket]int)} }

// bucketInput is the evidence a bucket is judged on.
type bucketInput struct {
	// runID is the sweep judging the bucket, "" outside a sweep.
	runID string
	// confirmed is the distinct confirmed subjects: this run's, or the rolling window's when probed < 0.
	confirmed int
	// probed is the entries probed on the bucket's provider this run, negative when no probe set the scale.
	probed int
	// held is how many of confirmed the breaker already held before this run.
	held int
	// sample is what the sweep found among the provider's entries that were not already held.
	sample sweepSample
}

// sweepSample is the entries a sweep probed on a provider that were not held by the breaker, and how many were
// broken. A large enough clean sample is evidence the provider works, standing in for a canary that had nothing
// recently good to probe.
type sweepSample struct {
	probed, broken int
}

const (
	sampleMinimum       = 3
	sampleBrokenPercent = 10
)

// passes reports a sample of at least sampleMinimum entries with fewer than sampleBrokenPercent percent broken.
func (s sweepSample) passes() bool {
	return s.probed >= sampleMinimum && s.broken*100 < s.probed*sampleBrokenPercent
}

type bucketOutcome struct {
	hold holdReason
	// newlyTripped reports a transition into tripped (not a refresh), for the notification.
	newlyTripped bool
	// released reports that this evaluation moved the bucket to releasing.
	released bool
	// draining reports a releasing bucket: entries the caps leave stay held for the next run.
	draining bool
}

// breaker holds the bucket machines, the rolling heal ledger and the canary verdicts. It is in memory: a
// restart reseeds the ledger and the window from the reacquire journal, and held entries stay on their rows.
type breaker struct {
	mu       sync.Mutex
	settings func() config.BreakerSettings
	now      func() time.Time
	canary   canaryFunc
	buckets  map[Bucket]*bucketBreaker
	ledger   []healRecord
	verdicts map[Bucket]canaryVerdict
	flight   flight.Group[canaryOutcome]
	// lifetime is the context a canary probe is tied to; nil leaves it to the caller's detached context.
	lifetime func() context.Context
	// track registers a probe in flight and returns the function that ends it; nil tracks nothing.
	track func() func()
}

// canaryProbeTimeout bounds one canary probe.
const canaryProbeTimeout = 2 * time.Minute

func newBreaker(settings func() config.BreakerSettings, canary canaryFunc) *breaker {
	return &breaker{
		settings: settings,
		now:      time.Now,
		canary:   canary,
		buckets:  make(map[Bucket]*bucketBreaker),
		verdicts: make(map[Bucket]canaryVerdict),
	}
}

func (b *breaker) bucketLocked(key Bucket) *bucketBreaker {
	bk := b.buckets[key]
	if bk == nil {
		bk = &bucketBreaker{state: breakerClosed, confirmed: make(map[string]*confirmation)}
		b.buckets[key] = bk
	}
	return bk
}

// over reports whether confirmed failures trip a bucket. With a probed scale it is the sweep rule: enough
// confirmed AND (a large share of the probed OR a large absolute count). Without one only the count applies.
func over(s config.BreakerSettings, confirmed, probed int) bool {
	if confirmed < s.MinConfirmed {
		return false
	}
	if probed < 0 {
		return true
	}
	return confirmed >= s.Confirmed || confirmed*100 >= probed*s.Percent
}

// confirmation is when a subject was first confirmed broken, and whether a heal for it is being held.
type confirmation struct {
	at   time.Time
	held bool
}

// noteConfirmed records that subject was confirmed broken in key's bucket, for the rolling window. Only the first
// sighting counts: a caller retrying a held heal is the same failure, not a new one, so retries cannot keep a
// bucket tripped.
func (b *breaker) noteConfirmed(key Bucket, subject string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	confirmed := b.bucketLocked(key).confirmed
	now := b.now()
	seen := confirmed[subject]
	switch {
	case seen == nil:
		confirmed[subject] = &confirmation{at: now}
	case !seen.held && seen.at.Before(now.Add(-b.settings().Window)):
		// Its sighting aged out of the window and it failed again: a new failure, not a retry.
		seen.at = now
	}
}

// markHeld records whether the heal for subject is being held, which keeps its sighting past the window so a
// retry is still recognised.
func (b *breaker) markHeld(key Bucket, subject string, held bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.bucketLocked(key).confirmed[subject]; c != nil {
		c.held = held
	}
}

// windowCount is the number of distinct subjects confirmed in key's bucket within the rolling window.
func (b *breaker) windowCount(key Bucket) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.windowCountLocked(key, b.settings())
}

func (b *breaker) windowCountLocked(key Bucket, s config.BreakerSettings) int {
	bk := b.bucketLocked(key)
	cutoff := b.now().Add(-s.Window)
	counted := 0
	for subject, c := range bk.confirmed {
		switch {
		case !c.at.Before(cutoff):
			counted++
		case !c.held:
			delete(bk.confirmed, subject)
		}
	}
	return counted
}

// evaluate judges key's bucket on in, moving it between states, and says whether its heals may go ahead.
// A canary, when the state calls for one and no fresh verdict is cached, runs outside the lock.
func (b *breaker) evaluate(ctx context.Context, key Bucket, in bucketInput) bucketOutcome {
	b.mu.Lock()
	out, needCanary := b.stepLocked(key, in, nil)
	b.mu.Unlock()
	if !needCanary {
		return out
	}
	verdict := b.probeCanary(ctx, key)
	b.mu.Lock()
	defer b.mu.Unlock()
	out, _ = b.stepLocked(key, in, &verdict)
	return out
}

// probeCanary runs the canary of key's bucket and caches its verdict. Concurrent callers for one bucket share a
// single probe, and the verdict is cached before any of them returns.
func (b *breaker) probeCanary(ctx context.Context, key Bucket) canaryOutcome {
	verdict, _, err := b.flight.Do(ctx, key.String(), func(ctx context.Context) (canaryOutcome, error) {
		// A caller that reached the flight after another's probe finished finds its verdict cached.
		b.mu.Lock()
		cached, ok := b.verdicts[key]
		fresh := ok && b.now().Sub(cached.at) < b.settings().CanaryCache
		b.mu.Unlock()
		if fresh {
			return cached.outcome, nil
		}
		verdict := canaryInconclusive
		if b.canary != nil {
			// The flight runs without its first caller's cancellation, so the probe is bounded here instead: by a
			// timeout, and by the service's lifetime, which Stop ends and then waits for the probe to leave.
			if b.track != nil {
				defer b.track()()
			}
			if b.lifetime != nil {
				ctx = b.lifetime()
			}
			probeCtx, cancel := context.WithTimeout(ctx, canaryProbeTimeout)
			verdict = b.canary(probeCtx, key, b.settings())
			cancel()
			if errors.Is(ctx.Err(), context.Canceled) {
				// The service stopped under the probe (a restart for a settings change, or shutdown). Whatever it
				// found says nothing about the provider and must not hold the bucket for the cache's length. A
				// timeout is different: the provider did not answer in time, and that is cached.
				return canaryAborted, nil
			}
		}
		b.mu.Lock()
		b.verdicts[key] = canaryVerdict{outcome: verdict, at: b.now()}
		b.mu.Unlock()
		return verdict, nil
	})
	if err != nil {
		return canaryInconclusive
	}
	return verdict
}

// stepLocked applies one evaluation. With needCanary true it changed nothing and wants a verdict.
func (b *breaker) stepLocked(key Bucket, in bucketInput, fresh *canaryOutcome) (bucketOutcome, bool) {
	s := b.settings()
	now := b.now()
	bk := b.bucketLocked(key)
	confirmed, probed := in.confirmed, in.probed
	switch {
	case probed < 0:
		confirmed = b.windowCountLocked(key, s)
	case bk.state == breakerReleasing:
		// The held rows are the drain's backlog, not new evidence, sample or not: a sweep after the release seldom
		// finds healthy entries due again, so only entries broken outside the hold can re-trip the bucket.
		confirmed, probed = confirmed-in.held, probed-in.held
	case bk.state != breakerClosed && in.sample.passes():
		// The provider works for the entries a sweep found that were not held, so the held ones still broken are the
		// failure that opened the bucket, not new evidence: judge the bucket on the sample alone. Without a clean
		// sample, and on a closed bucket (fresh after a restart), held rows count and keep or trip the hold.
		confirmed, probed = confirmed-in.held, in.sample.probed
	}
	exceeded := over(s, confirmed, probed)

	switch bk.state {
	case breakerClosed:
		if exceeded {
			return b.tripLocked(bk, now, in.runID, holdTripped), false
		}
		return b.canaryStepLocked(bk, key, in, s, fresh, false)
	case breakerTripped:
		if exceeded && in.probed < 0 {
			// The rolling window is its own clock: its old sightings age out, so a caller retrying within it must
			// not push the release further away.
			return bucketOutcome{hold: holdOf(bk)}, false
		}
		if exceeded {
			return b.tripLocked(bk, now, in.runID, holdTripped), false
		}
		if now.Sub(bk.since) < s.ReleaseAfter || (in.runID != "" && in.runID == bk.trippedRun) {
			return bucketOutcome{hold: holdOf(bk)}, false
		}
		return b.canaryStepLocked(bk, key, in, s, fresh, true)
	case breakerReleasing:
		if exceeded {
			return b.tripLocked(bk, now, in.runID, holdTripped), false
		}
		return bucketOutcome{draining: true}, false
	}
	return bucketOutcome{hold: holdTripped}, false
}

func (b *breaker) tripLocked(bk *bucketBreaker, now time.Time, runID string, reason holdReason) bucketOutcome {
	was := bk.state
	if err := bk.transition(breakerTripped, now, runID, reason); err != nil {
		return bucketOutcome{hold: holdTripped}
	}
	return bucketOutcome{hold: reason, newlyTripped: was != breakerTripped}
}

// canaryStepLocked decides on a bucket not over threshold from its canary: a pass (or no signal) admits and, for
// a tripped bucket, releases it; a failure trips it; an inconclusive canary holds this evaluation only.
func (b *breaker) canaryStepLocked(
	bk *bucketBreaker, key Bucket, in bucketInput, s config.BreakerSettings, fresh *canaryOutcome, releasing bool,
) (bucketOutcome, bool) {
	now := b.now()
	outcome, ok := canaryInconclusive, false
	switch cached, cachedOK := b.verdicts[key]; {
	case fresh != nil:
		outcome, ok = *fresh, true
	case cachedOK && now.Sub(cached.at) < s.CanaryCache:
		outcome, ok = cached.outcome, true
	}
	if !ok {
		return bucketOutcome{}, true
	}
	switch outcome {
	case canaryFail:
		return b.tripLocked(bk, now, in.runID, holdCanaryFailed), false
	case canaryInconclusive, canaryAborted:
		return bucketOutcome{hold: holdCanaryInconclusive}, false
	case canaryNone, canaryPass:
		if releasing && outcome == canaryNone && (in.runID == "" || !in.sample.passes()) {
			// Nothing to probe is no evidence of recovery: the clock alone never releases the bucket.
			bk.reason = holdNoCanary
			return bucketOutcome{hold: holdNoCanary}, false
		}
		if releasing {
			if err := bk.transition(breakerReleasing, now, in.runID, holdNone); err != nil {
				return bucketOutcome{hold: holdTripped}, false
			}
			return bucketOutcome{released: true, draining: true}, false
		}
		return bucketOutcome{}, false
	}
	return bucketOutcome{hold: holdCanaryInconclusive}, false
}

// holdOf is why a tripped bucket holds: the reason it was tripped or last held for.
func holdOf(bk *bucketBreaker) holdReason {
	if bk.reason == holdNone {
		return holdTripped
	}
	return bk.reason
}

// forceRelease closes a tripped bucket on an operator's say-so, but only one held as no_canary: that is the one
// hold waiting for a sweep or an operator. A bucket a canary or the threshold tripped is not released, since a
// force is no evidence the provider works. It reports whether the bucket was released and, when it was open and
// stays so, why it is held.
func (b *breaker) forceRelease(key Bucket) (bool, holdReason) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bk := b.buckets[key]
	if bk == nil || bk.state != breakerTripped {
		return false, holdNone
	}
	if bk.reason != holdNoCanary {
		return false, bk.reason
	}
	now := b.now()
	released := bk.transition(breakerReleasing, now, "", holdNone) == nil &&
		bk.transition(breakerClosed, now, "", holdNone) == nil
	return released, bk.reason
}

// settle closes every bucket that was releasing: its held entries have had their chance to drain.
func (b *breaker) settle(keys ...Bucket) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for _, key := range keys {
		if bk := b.buckets[key]; bk != nil && bk.state == breakerReleasing {
			_ = bk.transition(breakerClosed, now, "", holdNone)
		}
	}
}

// settleDrained closes, after a sweep, every releasing bucket of a provider the sweep probed that holds no entry
// the caps left for a later run: the sweep re-probed its held entries and admitted every one still broken.
func (b *breaker) settleDrained(probed map[string]int, undrained map[Bucket]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for key, bk := range b.buckets {
		if bk.state == breakerReleasing && probed[key.Provider] > 0 && !undrained[key] {
			_ = bk.transition(breakerClosed, now, "", holdNone)
		}
	}
}

// tryAdmit starts the heals in items if every bucket and the total are under their caps, in the run so far
// (usage, nil outside a run) and in the rolling window. A bucket admits while its count is under the cap, so an
// entry bigger than the cap still heals once.
func (b *breaker) tryAdmit(items []healItem, usage *runUsage) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.settings()
	now := b.now()
	b.pruneLedgerLocked(now, s)

	cost := make(map[Bucket]int, len(items))
	for _, item := range items {
		cost[item.bucket] += item.units
	}
	total := 0
	perBucket := make(map[Bucket]int)
	for _, rec := range b.ledger {
		total += rec.units
		perBucket[rec.bucket] += rec.units
	}
	if total >= s.CapPerRun || (usage != nil && usage.total >= s.CapPerRun) {
		return false
	}
	for key := range cost {
		if perBucket[key] >= s.CapPerBucket || (usage != nil && usage.perBucket[key] >= s.CapPerBucket) {
			return false
		}
	}
	b.recordLocked(now, items, usage)
	return true
}

// recordForced counts heals an operator forced past the breaker, so the caps still see them.
func (b *breaker) recordForced(items []healItem, usage *runUsage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recordLocked(b.now(), items, usage)
}

// refund takes back heals that were admitted but never started, newest first: the caps count only deletions.
func (b *breaker) refund(items []healItem) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, item := range items {
		remaining := item.units
		for i := len(b.ledger) - 1; i >= 0 && remaining > 0; i-- {
			rec := &b.ledger[i]
			if rec.bucket != item.bucket {
				continue
			}
			taken := min(rec.units, remaining)
			rec.units -= taken
			remaining -= taken
		}
	}
	b.ledger = slices.DeleteFunc(b.ledger, func(rec healRecord) bool { return rec.units <= 0 })
}

func (b *breaker) recordLocked(now time.Time, items []healItem, usage *runUsage) {
	for _, item := range items {
		b.ledger = append(b.ledger, healRecord{at: now, bucket: item.bucket, units: item.units})
		if usage != nil {
			usage.total += item.units
			usage.perBucket[item.bucket] += item.units
		}
	}
}

func (b *breaker) pruneLedgerLocked(now time.Time, s config.BreakerSettings) {
	cutoff := now.Add(-s.Window)
	b.ledger = slices.DeleteFunc(b.ledger, func(rec healRecord) bool { return rec.at.Before(cutoff) })
}

// seed restores one heal from before a restart into the ledger and the window.
func (b *breaker) seed(at time.Time, key Bucket, subject string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ledger = append(b.ledger, healRecord{at: at, bucket: key, units: 1})
	b.bucketLocked(key).confirmed[subject] = &confirmation{at: at}
}

// BucketStatus is the observable state of one bucket.
type BucketStatus struct {
	Provider string    `json:"provider"`
	Class    Class     `json:"class"`
	State    string    `json:"state"`
	Since    time.Time `json:"since,omitzero"`
	Reason   string    `json:"reason,omitempty"`
	// Hint says what a held bucket is waiting for, when that is not obvious.
	Hint string `json:"hint,omitempty"`
}

// status lists the buckets that are not closed.
func (b *breaker) status() []BucketStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []BucketStatus
	for key, bk := range b.buckets {
		if bk.state == breakerClosed {
			continue
		}
		out = append(out, BucketStatus{
			Provider: key.Provider,
			Class:    key.Class,
			State:    string(bk.state),
			Since:    bk.since,
			Reason:   string(bk.reason),
			Hint:     bk.reason.hint(),
		})
	}
	slices.SortFunc(out, func(a, c BucketStatus) int {
		return cmp.Or(cmp.Compare(a.Provider, c.Provider), cmp.Compare(a.Class, c.Class))
	})
	return out
}
