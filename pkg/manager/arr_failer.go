package manager

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const (
	maxRecheckAttempts = 5
	maxFailBackoff     = time.Hour
	maxBackoffShift    = 6
	failBackoffBase    = time.Minute
	ackWarnAfter       = time.Minute
	failWaitArrStatus  = "arr_not_registered"
	failWaitKeyStatus  = "other_job_active"
)

type resultKind uint8

const (
	resAcked resultKind = iota + 1
	resAckFailed
	resStale
	resHeld
	resNoArr
	resOtherJob
	resError
	resMoved
)

type attemptSpec struct {
	key, arr, provider string
	at                 time.Time
	recheck            bool
	attempt            uint64
	arrGen, keyGen     uint64
	jobID              string
}

func (s attemptSpec) bucket() string { return failBucket(s.arr, s.provider) }

type attemptResult struct {
	spec          attemptSpec
	kind          resultKind
	retryAt       time.Time
	arr, provider string
	jobID         string
	err           error
}

type failStatus struct {
	LastError string
	NextRetry time.Time
	Waiting   string
}

type arrFailerDeps struct {
	now            func() time.Time
	startAttempt   func(name string, fn func(ctx context.Context)) error
	attemptGrab    func(ctx context.Context, spec attemptSpec) attemptResult
	attemptRecheck func(ctx context.Context, spec attemptSpec) attemptResult
	attemptAck     func(ctx context.Context, spec attemptSpec) attemptResult
	onLoop         func()
	logger         zerolog.Logger
}

type bucketState struct {
	running   string
	attempt   uint64
	heldUntil time.Time
}

type arrFailer struct {
	deps     arrFailerDeps
	mu       sync.Mutex
	items    map[string]*failItem
	buckets  map[string]*bucketState
	arrGen   map[string]uint64
	keyGen   map[string]uint64
	attempts uint64
	wake     chan struct{}
	results  chan attemptResult
}

func newArrFailer(deps arrFailerDeps) *arrFailer {
	return &arrFailer{
		deps:    deps,
		items:   make(map[string]*failItem),
		buckets: make(map[string]*bucketState),
		arrGen:  make(map[string]uint64),
		keyGen:  make(map[string]uint64),
		wake:    make(chan struct{}, 1),
		results: make(chan attemptResult),
	}
}

func failProvider(entry *storage.Entry) string {
	if entry == nil || entry.ActiveProvider == "" {
		return "torrent"
	}
	return entry.ActiveProvider
}

func failBackoff(fails int) time.Duration {
	return min(failBackoffBase<<min(max(fails, 1)-1, maxBackoffShift), maxFailBackoff)
}

func keyGenName(arrName, downloadID string) string {
	return arrName + "\x00" + strings.ToUpper(downloadID)
}

func (f *arrFailer) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *arrFailer) newItem(key, arrName, provider string, at time.Time, recheck bool) *failItem {
	return &failItem{key: key, arr: arrName, provider: provider, at: at, recheck: recheck, due: f.deps.now()}
}

func (f *arrFailer) wantGrab(g grab.Grab) {
	if g.State.ArrFail.State != grab.ArrFailPending {
		return
	}
	key := strings.ToLower(g.State.Key)
	arrName, provider, at := g.State.ArrFail.Arr, failProvider(g.Entry), g.State.Failure.At
	f.mu.Lock()
	cur := f.items[key]
	switch {
	case cur == nil:
		f.items[key] = f.newItem(key, arrName, provider, at, false)
	case !cur.recheck && cur.at.Equal(at):
		f.mu.Unlock()
		return
	case cur.phase == failRunning:
		cur.superseded, cur.untold, cur.recheck = true, false, false
		cur.at, cur.arr, cur.provider, cur.jobID = at, arrName, provider, ""
	default:
		f.items[key] = f.newItem(key, arrName, provider, at, false)
	}
	f.mu.Unlock()
	f.poke()
}

func (f *arrFailer) wantRecheck(entry *storage.Entry, arrName string) {
	key := strings.ToLower(entry.InfoHash)
	f.mu.Lock()
	if _, ok := f.items[key]; ok {
		f.mu.Unlock()
		return
	}
	now := f.deps.now()
	f.items[key] = f.newItem(key, arrName, failProvider(entry), now, true)
	f.mu.Unlock()
	f.poke()
}

func (f *arrFailer) untell(key string, at time.Time) {
	key = strings.ToLower(key)
	f.mu.Lock()
	if it := f.items[key]; it != nil && !it.recheck && it.at.Equal(at) {
		if it.phase == failRunning {
			it.untold = true
		} else {
			delete(f.items, key)
		}
	}
	f.mu.Unlock()
	f.poke()
}

func (f *arrFailer) arrAdded(name string) {
	f.mu.Lock()
	f.arrGen[name]++
	now := f.deps.now()
	for _, it := range f.items {
		if it.wait == failWaitArr && it.arr == name {
			it.wait, it.due = failWaitNone, now
		}
	}
	f.mu.Unlock()
	f.poke()
}

func (f *arrFailer) keyReleased(arrName, downloadID string) {
	f.mu.Lock()
	f.keyGen[keyGenName(arrName, downloadID)]++
	now := f.deps.now()
	for _, it := range f.items {
		if it.wait == failWaitKey && it.arr == arrName && strings.EqualFold(it.key, downloadID) {
			it.wait, it.due = failWaitNone, now
		}
	}
	f.mu.Unlock()
	f.poke()
}

func (f *arrFailer) status(key string, at time.Time) (failStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[strings.ToLower(key)]
	if it == nil || it.recheck || !it.at.Equal(at) {
		return failStatus{}, false
	}
	st := failStatus{LastError: it.lastErr}
	switch it.wait {
	case failWaitArr:
		st.Waiting = failWaitArrStatus
	case failWaitKey:
		st.Waiting = failWaitKeyStatus
	case failWaitNone:
		if it.phase == failRunning {
			break
		}
		if it.fails > 0 {
			st.NextRetry = it.due
		}
		if b := f.buckets[it.bucket()]; b != nil && it.phase == failWaiting && b.heldUntil.After(f.deps.now()) {
			st.NextRetry = laterOf(st.NextRetry, b.heldUntil)
		}
	}
	return st, true
}

type launch struct {
	spec attemptSpec
	ack  bool
	from failItemPhase
}

func (f *arrFailer) bucket(name string) *bucketState {
	b := f.buckets[name]
	if b == nil {
		b = &bucketState{}
		f.buckets[name] = b
	}
	return b
}

func (f *arrFailer) runnable(now time.Time) []*failItem {
	var ready []*failItem
	for _, it := range f.items {
		if it.phase == failRunning || it.wait != failWaitNone || it.due.After(now) {
			continue
		}
		if b := f.buckets[it.bucket()]; b != nil && it.phase == failWaiting && b.heldUntil.After(now) {
			continue
		}
		ready = append(ready, it)
	}
	slices.SortFunc(ready, func(a, b *failItem) int {
		return cmp.Or(a.at.Compare(b.at), strings.Compare(a.key, b.key))
	})
	return ready
}

func (f *arrFailer) nextWake(now time.Time) time.Time {
	var next time.Time
	for _, it := range f.items {
		if it.phase == failRunning || it.wait != failWaitNone || f.bucket(it.bucket()).running != "" {
			continue
		}
		at := it.due
		if it.phase == failWaiting {
			at = laterOf(at, f.bucket(it.bucket()).heldUntil)
		}
		if at.After(now) && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	return next
}

func (f *arrFailer) plan() ([]launch, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.deps.now()
	var launches []launch
	for _, it := range f.runnable(now) {
		b := f.bucket(it.bucket())
		if b.running != "" {
			continue
		}
		from := it.phase
		if err := it.move(failRunning); err != nil {
			f.deps.logger.Error().Err(err).Str("key", it.key).Msg("Arr fail item refused a move")
			delete(f.items, it.key)
			continue
		}
		f.attempts++
		it.attempt = f.attempts
		b.running, b.attempt = it.key, it.attempt
		launches = append(launches, launch{
			spec: attemptSpec{
				key: it.key, arr: it.arr, provider: it.provider, at: it.at, recheck: it.recheck, attempt: it.attempt,
				arrGen: f.arrGen[it.arr], keyGen: f.keyGen[keyGenName(it.arr, it.key)], jobID: it.jobID,
			},
			ack:  from == failAckWaiting,
			from: from,
		})
	}
	next := f.nextWake(now)
	for name, b := range f.buckets {
		if b.running == "" && !b.heldUntil.After(now) {
			delete(f.buckets, name)
		}
	}
	return launches, next
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (f *arrFailer) run(ctx context.Context) error {
	for {
		launches, next := f.plan()
		for _, l := range launches {
			f.launch(l)
		}
		if stopped(ctx) {
			return nil
		}
		if f.deps.onLoop != nil {
			f.deps.onLoop()
		}
		var timer *time.Timer
		var fire <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(next.Sub(f.deps.now()))
			fire = timer.C
		}
		select {
		case <-f.wake:
		case res := <-f.results:
			f.apply(res)
		case <-fire:
		case <-ctx.Done():
		}
		if timer != nil {
			timer.Stop()
		}
		if stopped(ctx) {
			return nil
		}
	}
}

func (f *arrFailer) launch(l launch) {
	spec := l.spec
	err := f.deps.startAttempt("arr-fail", func(ctx context.Context) { f.runAttempt(ctx, spec, l.ack) })
	if err != nil {
		f.refused(l, err)
	}
}

func (f *arrFailer) runAttempt(ctx context.Context, spec attemptSpec, ack bool) {
	res := attemptResult{spec: spec, kind: resError}
	defer func() {
		if r := recover(); r != nil {
			res = attemptResult{spec: spec, kind: resError, err: fmt.Errorf("attempt panicked: %v", r)}
		}
		select {
		case f.results <- res:
		case <-ctx.Done():
		}
	}()
	switch {
	case ack:
		res = f.deps.attemptAck(ctx, spec)
	case spec.recheck:
		res = f.deps.attemptRecheck(ctx, spec)
	default:
		res = f.deps.attemptGrab(ctx, spec)
	}
	res.spec = spec
}

func (f *arrFailer) release(spec attemptSpec) {
	if b := f.buckets[spec.bucket()]; b != nil && b.running == spec.key && b.attempt == spec.attempt {
		b.running, b.attempt = "", 0
	}
}

func (f *arrFailer) refused(l launch, cause error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.release(l.spec)
	it := f.items[l.spec.key]
	if it == nil || it.phase != failRunning || it.attempt != l.spec.attempt {
		return
	}
	if err := it.move(l.from); err != nil {
		delete(f.items, it.key)
		return
	}
	it.fails++
	it.lastErr = cause.Error()
	it.due = f.deps.now().Add(failBackoff(it.fails))
}

func (f *arrFailer) apply(res attemptResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.deps.now()
	spec := res.spec
	f.release(spec)
	if res.kind == resHeld && res.retryAt.After(now) {
		b := f.bucket(spec.bucket())
		b.heldUntil = laterOf(b.heldUntil, res.retryAt)
	}
	it := f.items[spec.key]
	if it == nil || it.phase != failRunning || it.attempt != spec.attempt {
		return
	}
	if it.untold && !it.superseded {
		delete(f.items, it.key)
		return
	}
	target := failWaiting
	if res.kind == resAckFailed && !it.superseded {
		target = failAckWaiting
	}
	if err := it.move(target); err != nil {
		f.deps.logger.Error().Err(err).Str("key", it.key).Msg("Arr fail item refused a move")
		delete(f.items, it.key)
		return
	}
	it.wait, it.due = failWaitNone, now
	if it.superseded {
		it.superseded, it.fails, it.lastErr, it.ackSince, it.ackWarned = false, 0, "", time.Time{}, false
		return
	}
	if res.err != nil {
		it.lastErr = res.err.Error()
	}
	f.settle(it, res, now)
}

func (f *arrFailer) settle(it *failItem, res attemptResult, now time.Time) {
	spec := res.spec
	switch res.kind {
	case resHeld:
		if !res.retryAt.After(now) {
			it.due = now.Add(failBackoff(it.fails + 1))
			if !it.recheck {
				it.fails++
			}
		}
	case resNoArr:
		if f.arrGen[spec.arr] == spec.arrGen {
			it.wait = failWaitArr
		}
	case resOtherJob:
		if f.keyGen[keyGenName(spec.arr, spec.key)] == spec.keyGen {
			it.wait = failWaitKey
		}
	case resError:
		it.fails++
		if it.recheck && it.fails >= maxRecheckAttempts {
			f.deps.logger.Warn().Err(res.err).Str("infohash", it.key).Int("attempts", maxRecheckAttempts).
				Msg("Failed to fail a held grab in the Arr; giving up on it")
			delete(f.items, it.key)
			return
		}
		it.due = now.Add(failBackoff(it.fails))
	case resAckFailed:
		f.ackFailed(it, res, now)
	case resMoved:
		it.arr, it.provider = res.arr, res.provider
	case resAcked, resStale:
		delete(f.items, it.key)
	}
}

func (f *arrFailer) ackFailed(it *failItem, res attemptResult, now time.Time) {
	it.jobID = res.jobID
	if it.ackSince.IsZero() {
		it.ackSince, it.fails = now, 0
	}
	it.fails++
	it.due = now.Add(failBackoff(it.fails))
	if !it.ackWarned && !now.Before(it.ackSince.Add(ackWarnAfter)) {
		it.ackWarned = true
		f.deps.logger.Warn().Err(res.err).Str("infohash", it.key).
			Msg("The Arr was told but the grab could not record it; trying again")
	}
}
