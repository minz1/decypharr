package arr

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// CanaryVerdict is the answer of a canary probe of one provider.
type CanaryVerdict int

const (
	// CanaryNone is no signal: nothing recently good exists to probe.
	CanaryNone CanaryVerdict = iota
	CanaryPass
	CanaryFail
	// CanaryInconclusive neither passed nor showed the provider failing.
	CanaryInconclusive
)

// QueueGuardHooks is what the manager lends the queue guard. Each hook may be nil; a nil Canary reads as an
// inconclusive canary, which holds the downloads that need one.
type QueueGuardHooks struct {
	// Provider names the provider of decypharr's own entry for a download ID, "" when it has none.
	Provider func(downloadID string) string
	// Canary probes recently good entries of a provider. It stands in for the repair canary, which this package
	// cannot import.
	Canary func(ctx context.Context, provider string) CanaryVerdict
	// Notify announces that a bucket was tripped.
	Notify func(subject, message string)
}

// SetQueueGuardHooks installs the manager's hooks for the queue guard.
func (s *Service) SetQueueGuardHooks(hooks QueueGuardHooks) {
	s.guard.mu.Lock()
	defer s.guard.mu.Unlock()
	s.guard.hooks = hooks
}

// failedDownloadRule is the one rule whose excess can still pass: a failed download is evidence of the release only
// while the provider itself is healthy, which the canary tests.
const failedDownloadRule = "failed_download"

// guardState is the state of one blocklist bucket.
//
//	closed   downloads pass within the limits
//	tripped  a canary found the provider failing, or could not tell; downloads beyond the free limit are held
//	         until a later canary passes
type guardState string

const (
	guardClosed  guardState = "closed"
	guardTripped guardState = "tripped"
)

// successors lists every state a bucket may move to from s; it is the whole transition table.
func (s guardState) successors() []guardState {
	switch s {
	case guardClosed:
		return []guardState{guardTripped}
	case guardTripped:
		return []guardState{guardClosed}
	}
	return nil
}

var errGuardEdge = errors.New("illegal queue guard transition")

// guardBucket is the state of one (arr, rule, provider) bucket and the downloads it blocklisted in the window.
type guardBucket struct {
	state guardState
	since time.Time
	// sent holds when each distinct download was blocklisted, never when a row was.
	sent map[string]time.Time
	// warnedAt is when a hold in this bucket was last logged, so a long hold logs once per window.
	warnedAt time.Time
}

// transition is the only way a bucket changes state.
func (b *guardBucket) transition(to guardState, at time.Time) error {
	if !slices.Contains(b.state.successors(), to) {
		return fmt.Errorf("%w: %s -> %s", errGuardEdge, b.state, to)
	}
	b.state, b.since = to, at
	return nil
}

type guardKey struct{ arr, rule, provider string }

func (k guardKey) String() string { return k.arr + "/" + k.rule + "/" + k.provider }

type guardVerdict struct {
	verdict CanaryVerdict
	at      time.Time
}

// queueGuard limits how fast queue cleanup blocklists downloads. It counts distinct downloads, never queue rows
// (a season pack has one row per episode), and only those whose blocklist was sent. It is in memory: a restart
// starts a fresh window.
type queueGuard struct {
	mu      sync.Mutex
	hooks   QueueGuardHooks
	buckets map[guardKey]*guardBucket
	// arrSent holds, per arr, when each distinct download was blocklisted, across all rules.
	arrSent map[string]map[string]time.Time
	// heldSince is when a download was first held, so the next window serves the oldest first.
	heldSince map[string]time.Time
	// arrWarned is when an arr's hard cap was last logged.
	arrWarned map[string]time.Time
	verdicts  map[string]guardVerdict
}

func newQueueGuard() *queueGuard {
	return &queueGuard{
		buckets:   make(map[guardKey]*guardBucket),
		arrSent:   make(map[string]map[string]time.Time),
		heldSince: make(map[string]time.Time),
		arrWarned: make(map[string]time.Time),
		verdicts:  make(map[string]guardVerdict),
	}
}

func (g *queueGuard) bucketLocked(key guardKey) *guardBucket {
	bucket := g.buckets[key]
	if bucket == nil {
		bucket = &guardBucket{state: guardClosed, sent: make(map[string]time.Time)}
		g.buckets[key] = bucket
	}
	return bucket
}

// QueueGuardBucket is the observable state of a bucket that is not closed.
type QueueGuardBucket struct {
	Arr      string    `json:"arr"`
	Rule     string    `json:"rule"`
	Provider string    `json:"provider"`
	State    string    `json:"state"`
	Since    time.Time `json:"since,omitzero"`
}

// QueueGuardStatus lists the buckets whose excess is held by a canary.
func (s *Service) QueueGuardStatus() []QueueGuardBucket {
	s.guard.mu.Lock()
	defer s.guard.mu.Unlock()
	var out []QueueGuardBucket
	for key, bucket := range s.guard.buckets {
		if bucket.state != guardClosed {
			out = append(out, QueueGuardBucket{
				Arr: key.arr, Rule: key.rule, Provider: key.provider, State: string(bucket.state), Since: bucket.since,
			})
		}
	}
	slices.SortFunc(out, func(a, b QueueGuardBucket) int {
		return cmp.Or(cmp.Compare(a.Arr, b.Arr), cmp.Compare(a.Rule, b.Rule), cmp.Compare(a.Provider, b.Provider))
	})
	return out
}

// downloadKey identifies a download for counting; a row with no download ID counts as its own.
func downloadKey(item QueueSchema) string {
	if item.DownloadID != "" {
		return strings.ToLower(item.DownloadID)
	}
	return fmt.Sprintf("row:%d", item.ID)
}

// guardProvider names the provider a download is judged under: decypharr's own entry for it, else its protocol.
func (g *queueGuard) guardProvider(item QueueSchema) string {
	g.mu.Lock()
	lookup := g.hooks.Provider
	g.mu.Unlock()
	if lookup != nil && item.DownloadID != "" {
		if provider := lookup(item.DownloadID); provider != "" {
			return provider
		}
	}
	if protocol := strings.ToLower(strings.TrimSpace(item.Protocol)); protocol != "" {
		return protocol
	}
	return "unknown"
}

// guardUnit is one download in one bucket with the rows that belong to it.
type guardUnit struct {
	key      guardKey
	download string
	rows     []int
}

func groupUnits(arrName string, items []QueueSchema) []*guardUnit {
	var units []*guardUnit
	index := make(map[string]*guardUnit)
	for _, item := range items {
		key := guardKey{arr: arrName, rule: item.rule, provider: item.provider}
		id := key.String() + "\x00" + downloadKey(item)
		unit := index[id]
		if unit == nil {
			unit = &guardUnit{key: key, download: downloadKey(item)}
			index[id] = unit
			units = append(units, unit)
		}
		unit.rows = append(unit.rows, item.ID)
	}
	return units
}

// guardEvent is something to log or announce once the guard's lock is released.
type guardEvent struct {
	trip bool
	key  guardKey
	text string
}

type guardDecision struct {
	allowed map[int]bool
	events  []guardEvent
}

// holdExcessBlocklists drops from both lists the rows the guard will not blocklist this tick.
func (s *Service) holdExcessBlocklists(
	ctx context.Context, name string, research, plain []QueueSchema,
) ([]QueueSchema, []QueueSchema) {
	allowed := s.admitBlocklists(ctx, name, slices.Concat(research, plain))
	return keepAllowed(research, allowed), keepAllowed(plain, allowed)
}

// admitBlocklists returns the queue rows of items that may be blocklisted now. The rest stay in the queue and
// are served first, oldest first, when the window has room.
func (s *Service) admitBlocklists(ctx context.Context, name string, items []QueueSchema) map[int]bool {
	if len(items) == 0 {
		return nil
	}
	g := s.guard
	units := groupUnits(name, items)
	limits := s.config.Get().QueueCleanup.BlocklistLimits()
	ttl := s.config.Get().Repair.BreakerSettings().CanaryCache

	fresh := make(map[string]CanaryVerdict)
	var decision guardDecision
	for {
		var need []string
		decision, need = g.decide(name, units, limits, ttl, s.now(), fresh)
		if len(need) == 0 {
			break
		}
		for _, provider := range need {
			fresh[provider] = s.canaryFor(ctx, provider)
		}
	}
	for _, event := range decision.events {
		s.logger.Warn().Str("bucket", event.key.String()).Msg(event.text)
		if event.trip {
			s.notifyGuard(event.key, event.text)
		}
	}
	return decision.allowed
}

func (s *Service) canaryFor(ctx context.Context, provider string) CanaryVerdict {
	s.guard.mu.Lock()
	probe := s.guard.hooks.Canary
	s.guard.mu.Unlock()
	verdict := CanaryInconclusive
	if probe != nil {
		verdict = probe(ctx, provider)
	}
	s.guard.mu.Lock()
	s.guard.verdicts[provider] = guardVerdict{verdict: verdict, at: s.now()}
	s.guard.mu.Unlock()
	return verdict
}

func (s *Service) notifyGuard(key guardKey, text string) {
	s.guard.mu.Lock()
	notify := s.guard.hooks.Notify
	s.guard.mu.Unlock()
	if notify != nil {
		notify("Queue cleanup held", fmt.Sprintf("\n**Bucket**: %s\n%s", key, text))
	}
}

func (u *guardUnit) id() string { return u.key.String() + "\x00" + u.download }

// compareHeld orders downloads held before ahead of new ones, the longest held first.
func compareHeld(heldSince map[string]time.Time, a, b *guardUnit) int {
	at, aHeld := heldSince[a.id()]
	bt, bHeld := heldSince[b.id()]
	switch {
	case aHeld && bHeld:
		return at.Compare(bt)
	case aHeld:
		return -1
	case bHeld:
		return 1
	}
	return 0
}

// pruneLocked forgets blocklists older than the window, and held marks of downloads that have not been seen since.
func (g *queueGuard) pruneLocked(arrName string, now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	for key, bucket := range g.buckets {
		if key.arr != arrName {
			continue
		}
		for download, at := range bucket.sent {
			if at.Before(cutoff) {
				delete(bucket.sent, download)
			}
		}
	}
	for download, at := range g.arrSent[arrName] {
		if at.Before(cutoff) {
			delete(g.arrSent[arrName], download)
		}
	}
	for id, at := range g.heldSince {
		if now.Sub(at) > 24*time.Hour {
			delete(g.heldSince, id)
		}
	}
}

// commit counts the downloads of items whose blocklist was sent (or may have been).
func (g *queueGuard) commit(arrName string, items []QueueSchema, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, item := range items {
		if item.rule == "" {
			continue
		}
		key := guardKey{arr: arrName, rule: item.rule, provider: item.provider}
		download := downloadKey(item)
		g.bucketLocked(key).sent[download] = now
		if g.arrSent[arrName] == nil {
			g.arrSent[arrName] = make(map[string]time.Time)
		}
		g.arrSent[arrName][download] = now
	}
}

// keepAllowed returns the items whose rows the guard admitted.
func keepAllowed(items []QueueSchema, allowed map[int]bool) []QueueSchema {
	kept := make([]QueueSchema, 0, len(items))
	for _, item := range items {
		if allowed[item.ID] {
			kept = append(kept, item)
		}
	}
	return kept
}
