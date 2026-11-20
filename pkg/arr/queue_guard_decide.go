package arr

import (
	"fmt"
	"slices"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// guardPlan is what one tick found for one bucket: the downloads beyond its free allowance and, for a rule that has
// one, the canary verdict that decides them.
type guardPlan struct {
	excess    []*guardUnit
	admits    int // new downloads taken from the bucket's free allowance
	verdict   CanaryVerdict
	canary    bool
	verdictAt time.Time
}

// admitsExcess reports whether the bucket's excess may go ahead: only for a rule with a canary, and only while it
// passes. A canary with nothing to probe is no evidence, so the excess waits for the next window.
func (p *guardPlan) admitsExcess() bool {
	return p.canary && p.verdict == CanaryPass
}

// decide judges every bucket with downloads this tick. With a non-empty need it changed nothing and wants those
// providers' canary verdicts in fresh.
func (g *queueGuard) decide(
	arrName string,
	units []*guardUnit,
	limits config.BlocklistLimits,
	ttl time.Duration,
	now time.Time,
	fresh map[string]CanaryVerdict,
) (guardDecision, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(arrName, now, limits.Window)
	g.releaseDrainedLocked(arrName, limits, now)

	// Priority: downloads held before, oldest first, then the rest in queue order.
	ordered := slices.Clone(units)
	slices.SortStableFunc(ordered, func(a, b *guardUnit) int { return compareHeld(g.heldSince, a, b) })

	plans := g.planBucketsLocked(ordered, limits)
	if need := g.pendingVerdictsLocked(plans, ttl, now, fresh); len(need) > 0 {
		return guardDecision{}, need
	}

	held := make(map[*guardUnit]time.Time)
	events := g.applyPlansLocked(plans, limits, ttl, now, held)
	events = append(events, g.capArrLocked(arrName, ordered, limits, now, held)...)
	retryAt := make(map[int]time.Time)
	for unit, at := range held {
		for _, row := range unit.rows {
			retryAt[row] = at
		}
	}
	return guardDecision{allowed: g.settleLocked(arrName, ordered, held, now), events: events, retryAt: retryAt}, nil
}

// releaseDrainedLocked closes a tripped bucket once its window has drained below the free allowance again.
func (g *queueGuard) releaseDrainedLocked(arrName string, limits config.BlocklistLimits, now time.Time) {
	for key, bucket := range g.buckets {
		if key.arr == arrName && bucket.state == guardTripped && len(bucket.sent) < limits.PerBucket {
			_ = bucket.transition(guardClosed, now)
		}
	}
}

func (g *queueGuard) planBucketsLocked(ordered []*guardUnit, limits config.BlocklistLimits) map[guardKey]*guardPlan {
	plans := make(map[guardKey]*guardPlan)
	for _, unit := range ordered {
		bucket := g.bucketLocked(unit.key)
		plan := plans[unit.key]
		if plan == nil {
			plan = &guardPlan{canary: ruleHasCanary(unit.key.rule)}
			plans[unit.key] = plan
		}
		switch {
		case isSent(bucket, unit):
			// Already counted this window: a retry costs nothing.
		case len(bucket.sent)+plan.admits < limits.PerBucket:
			plan.admits++
		default:
			plan.excess = append(plan.excess, unit)
		}
	}
	return plans
}

func isSent(bucket *guardBucket, unit *guardUnit) bool {
	_, ok := bucket.sent[unit.download]
	return ok
}

// pendingVerdictsLocked fills each plan's verdict from fresh or the cache and lists the providers still unknown.
func (g *queueGuard) pendingVerdictsLocked(
	plans map[guardKey]*guardPlan, ttl time.Duration, now time.Time, fresh map[string]CanaryVerdict,
) []string {
	var need []string
	for key, plan := range plans {
		if len(plan.excess) == 0 || !plan.canary {
			continue
		}
		if verdict, ok := fresh[key.provider]; ok {
			plan.verdict, plan.verdictAt = verdict, now
			continue
		}
		if cached, ok := g.verdicts[key.provider]; ok && now.Sub(cached.at) < ttl {
			plan.verdict, plan.verdictAt = cached.verdict, cached.at
			continue
		}
		if !slices.Contains(need, key.provider) {
			need = append(need, key.provider)
		}
	}
	return need
}

// applyPlansLocked moves buckets between states on their verdicts and marks the excess that is held.
func (g *queueGuard) applyPlansLocked(
	plans map[guardKey]*guardPlan, limits config.BlocklistLimits, ttl time.Duration, now time.Time,
	held map[*guardUnit]time.Time,
) []guardEvent {
	var events []guardEvent
	for key, plan := range plans {
		bucket := g.bucketLocked(key)
		if plan.canary && len(plan.excess) > 0 {
			events = append(events, g.applyVerdictLocked(key, bucket, plan, limits, now)...)
		}
		if plan.admitsExcess() {
			continue
		}
		retry := g.retryAtLocked(bucket, plan, limits, ttl, now)
		for _, unit := range plan.excess {
			held[unit] = retry
		}
		if len(plan.excess) > 0 && !plan.canary && now.Sub(bucket.warnedAt) >= limits.Window {
			bucket.warnedAt = now
			events = append(events, guardEvent{key: key, text: fmt.Sprintf(
				"Queue cleanup: more than %d distinct downloads an hour match this rule; "+
					"holding %d in the queue for the next window", limits.PerBucket, len(plan.excess))})
		}
	}
	return events
}

func (g *queueGuard) applyVerdictLocked(
	key guardKey, bucket *guardBucket, plan *guardPlan, limits config.BlocklistLimits, now time.Time,
) []guardEvent {
	switch {
	case plan.admitsExcess() && bucket.state == guardTripped:
		_ = bucket.transition(guardClosed, now)
	case (plan.verdict == CanaryFail || plan.verdict == CanaryInconclusive) && bucket.state == guardClosed:
		if err := bucket.transition(guardTripped, now); err == nil {
			return []guardEvent{{trip: true, key: key, text: fmt.Sprintf(
				"Queue cleanup: more than %d distinct downloads an hour and the provider canary did not pass; "+
					"holding the rest in the queue until it does", limits.PerBucket)}}
		}
	}
	return nil
}

// capArrLocked holds what would pass the arr-wide hard cap, in priority order; downloads already counted this
// window cost nothing.
func (g *queueGuard) capArrLocked(
	arrName string, ordered []*guardUnit, limits config.BlocklistLimits, now time.Time, held map[*guardUnit]time.Time,
) []guardEvent {
	counted := g.arrSent[arrName]
	remaining := max(limits.PerArr-len(counted), 0)
	capped := 0
	for _, unit := range ordered {
		if _, isHeld := held[unit]; isHeld {
			continue
		}
		if _, ok := counted[unit.download]; ok {
			continue
		}
		if remaining == 0 {
			held[unit] = oldestSentFree(counted, limits.Window, now)
			capped++
			continue
		}
		remaining--
	}
	if capped == 0 || now.Sub(g.arrWarned[arrName]) < limits.Window {
		return nil
	}
	g.arrWarned[arrName] = now
	return []guardEvent{{
		key: guardKey{arr: arrName, rule: "*", provider: "*"},
		text: fmt.Sprintf("Queue cleanup: %d distinct downloads blocklisted in the last window is the cap; "+
			"holding %d in the queue for the next window", limits.PerArr, capped),
	}}
}

// settleLocked remembers when each held download was first held and returns the queue rows of those that go ahead.
func (g *queueGuard) settleLocked(
	arrName string, ordered []*guardUnit, held map[*guardUnit]time.Time, now time.Time,
) map[int]bool {
	allowed := make(map[int]bool)
	for _, unit := range ordered {
		id := unit.id()
		if _, isHeld := held[unit]; isHeld {
			if _, seen := g.heldSince[id]; !seen {
				g.heldSince[id] = now
			}
			continue
		}
		delete(g.heldSince, id)
		g.reserveLocked(arrName, unit, now)
		for _, row := range unit.rows {
			allowed[row] = true
		}
	}
	return allowed
}

// reserveLocked counts an admitted download at once, so callers admitted meanwhile are judged against it; commit
// confirms it once its blocklist is sent and release takes it back if the blocklist was not.
func (g *queueGuard) reserveLocked(arrName string, unit *guardUnit, now time.Time) {
	bucket := g.bucketLocked(unit.key)
	if _, counted := bucket.sent[unit.download]; counted {
		return
	}
	bucket.sent[unit.download] = now
	if g.arrSent[arrName] == nil {
		g.arrSent[arrName] = make(map[string]time.Time)
	}
	if _, counted := g.arrSent[arrName][unit.download]; !counted {
		g.arrSent[arrName][unit.download] = now
	}
	g.pending[unit.id()] = struct{}{}
}

func oldestSentFree(sent map[string]time.Time, window time.Duration, now time.Time) time.Time {
	oldest := now
	for _, at := range sent {
		if at.Before(oldest) {
			oldest = at
		}
	}
	return oldest.Add(window + time.Nanosecond)
}

func (g *queueGuard) retryAtLocked(
	bucket *guardBucket, plan *guardPlan, limits config.BlocklistLimits, ttl time.Duration, now time.Time,
) time.Time {
	bucketFree := oldestSentFree(bucket.sent, limits.Window, now)
	if !plan.canary {
		return bucketFree
	}
	if plan.verdict == CanaryAborted {
		if len(bucket.sent) > 0 {
			return bucketFree
		}
		return now.Add(ttl)
	}
	return minTime(bucketFree, plan.verdictAt.Add(ttl))
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
