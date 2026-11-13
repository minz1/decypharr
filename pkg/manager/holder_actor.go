package manager

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

const (
	holderMaxRetries     = 3
	holderGoneFor        = time.Hour
	holderIdleAfter      = time.Hour
	holderRetryBase      = 10 * time.Second
	holderRetryCap       = 2 * time.Minute
	holderRetryDoublings = 4
)

type holderPhase uint8

const (
	holderIdle holderPhase = iota
	holderHeld
	holderDeleting
	holderGone
)

func allHolderPhases() []holderPhase {
	return []holderPhase{holderIdle, holderHeld, holderDeleting, holderGone}
}

func (p holderPhase) String() string {
	switch p {
	case holderIdle:
		return "idle"
	case holderHeld:
		return "held"
	case holderDeleting:
		return "deleting"
	case holderGone:
		return "gone"
	}
	return fmt.Sprintf("holder_phase(%d)", uint8(p))
}

func holderSuccessors(p holderPhase) []holderPhase {
	switch p {
	case holderIdle:
		return []holderPhase{holderHeld, holderDeleting}
	case holderHeld:
		return []holderPhase{holderIdle, holderDeleting}
	case holderDeleting:
		return []holderPhase{holderIdle, holderHeld, holderGone}
	case holderGone:
		return []holderPhase{holderIdle}
	}
	return nil
}

type releaseOutcome uint8

const (
	releaseRemoved releaseOutcome = iota
	releaseHeldElsewhere
	releaseFailed
	releaseStopped
	releaseReleased
)

func (o releaseOutcome) String() string {
	return [...]string{"removed", "held_elsewhere", "failed", "stopped", "released"}[o]
}

type releaseResult struct {
	outcome releaseOutcome
	err     error
}

type holderEventKind uint8

const (
	holderHold holderEventKind = iota
	holderRelease
	holderDeleteDone
	holderRetry
	holderForget
)

func (k holderEventKind) String() string {
	return [...]string{"hold", "release", "delete_done", "retry", "forget"}[k]
}

type holderEvent struct {
	kind holderEventKind
	grab string
	// del, library and slot describe a Release: the grab asks for the delete, its own library record is the one being
	// removed, and the placement held a download slot.
	del, library, slot bool
	done               chan releaseResult
	// outcome, err and stale describe a deleteDone: how the child ended, and the holds its recount found dead.
	outcome releaseOutcome
	err     error
	stale   []string
	at      time.Time
}

type holderCall struct {
	releasing     string
	library, slot bool
}

type holderState struct {
	phase   holderPhase
	holds   map[string]struct{}
	back    map[string]struct{}
	retries int
	waiters []chan releaseResult
	silent  bool
	call    holderCall
	joined  []holderEvent
	goneAt  time.Time
}

type holderEffectKind uint8

const (
	fxHolderDelete holderEffectKind = iota
	fxHolderAnswer
	fxHolderGone
	fxHolderRetry
	fxHolderSlot
	fxHolderForget
)

type holderEffect struct {
	kind   holderEffectKind
	grab   string
	ch     chan releaseResult
	result releaseResult
	call   holderCall
	holds  []string
	n      int
}

func cloneSet(s map[string]struct{}) map[string]struct{} {
	if s == nil {
		return make(map[string]struct{})
	}
	return maps.Clone(s)
}

func answer(ch chan releaseResult, outcome releaseOutcome, err error) []holderEffect {
	if ch == nil {
		return nil
	}
	return []holderEffect{{kind: fxHolderAnswer, ch: ch, result: releaseResult{outcome: outcome, err: err}}}
}

func (st holderState) moveTo(p holderPhase) (holderState, error) {
	if st.phase == p {
		return st, nil
	}
	if !state.Legal(holderSuccessors, st.phase, p) {
		return st, fmt.Errorf("holder: no move from %s to %s", st.phase, p)
	}
	st.phase = p
	return st, nil
}

func (st holderState) settle() (holderState, error) {
	if len(st.holds) > 0 {
		return st.moveTo(holderHeld)
	}
	return st.moveTo(holderIdle)
}

// holderStep is the one transition function of a holder: it returns the state an event leaves and the effects its
// owner applies, or the unchanged state and an error for a move the table does not list.
func holderStep(st holderState, ev holderEvent) (holderState, []holderEffect, error) {
	prev := st
	st.holds, st.back = cloneSet(st.holds), cloneSet(st.back)
	var fx []holderEffect
	var err error
	switch ev.kind {
	case holderHold:
		st, fx, err = holding(st, ev)
	case holderRelease:
		st, fx, err = releasing(st, ev)
	case holderDeleteDone:
		st, fx, err = deleteEnded(st, ev)
	case holderRetry:
		if st.phase == holderDeleting {
			fx = []holderEffect{{kind: fxHolderDelete, call: st.call, holds: sortedHolds(st.holds)}}
		}
	case holderForget:
		if st.phase == holderGone {
			st, err = st.moveTo(holderIdle)
			st.goneAt = time.Time{}
		}
	}
	if err != nil {
		return prev, nil, err
	}
	return st, fx, nil
}

func sortedHolds(s map[string]struct{}) []string {
	return slices.Sorted(maps.Keys(s))
}

func holding(st holderState, ev holderEvent) (holderState, []holderEffect, error) {
	switch st.phase {
	case holderGone:
		return st, []holderEffect{{kind: fxHolderGone, grab: ev.grab}}, nil
	case holderDeleting:
		st.back[ev.grab] = struct{}{}
		var fx []holderEffect
		st.joined = slices.DeleteFunc(slices.Clone(st.joined), func(j holderEvent) bool {
			if j.grab != ev.grab {
				return false
			}
			fx = append(fx, answer(j.done, releaseHeldElsewhere, nil)...)
			return true
		})
		return st, fx, nil
	case holderIdle, holderHeld:
	}
	st.holds[ev.grab] = struct{}{}
	next, err := st.moveTo(holderHeld)
	return next, nil, err
}

func releasing(st holderState, ev holderEvent) (holderState, []holderEffect, error) {
	switch st.phase {
	case holderGone:
		if ev.del {
			return st, answer(ev.done, releaseRemoved, nil), nil
		}
		return st, answer(ev.done, releaseReleased, nil), nil
	case holderDeleting:
		delete(st.holds, ev.grab)
		delete(st.back, ev.grab)
		if !ev.del {
			return st, answer(ev.done, releaseReleased, nil), nil
		}
		if ev.grab != st.call.releasing || ev.library != st.call.library {
			st.joined = append(slices.Clone(st.joined), ev)
			return st, nil, nil
		}
		if ev.done != nil {
			st.waiters = append(slices.Clone(st.waiters), ev.done)
		} else {
			st.silent = true
		}
		st.call.slot = st.call.slot || ev.slot
		return st, nil, nil
	case holderIdle, holderHeld:
	}
	delete(st.holds, ev.grab)
	if !ev.del {
		next, err := st.settle()
		return next, answer(ev.done, releaseReleased, nil), err
	}
	st.call = holderCall{releasing: ev.grab, library: ev.library, slot: ev.slot}
	st.retries, st.joined = 0, nil
	st.waiters, st.silent = nil, ev.done == nil
	if ev.done != nil {
		st.waiters = []chan releaseResult{ev.done}
	}
	next, err := st.moveTo(holderDeleting)
	return next, []holderEffect{{kind: fxHolderDelete, call: next.call, holds: sortedHolds(next.holds)}}, err
}

func deleteEnded(st holderState, ev holderEvent) (holderState, []holderEffect, error) {
	if st.phase != holderDeleting {
		return st, nil, fmt.Errorf("holder: delete ended in %s", st.phase)
	}
	for _, g := range ev.stale {
		delete(st.holds, g)
	}
	if ev.outcome == releaseHeldElsewhere && len(st.joined) > 0 {
		return recountJoined(st, ev)
	}
	if ev.outcome == releaseFailed {
		return failedAttempt(st, ev)
	}
	st = foldJoined(st)
	if ev.outcome == releaseRemoved {
		return deleted(st, ev)
	}
	fx := answerAll(st.waiters, ev.outcome, ev.err)
	st.waiters = nil
	return settled(st, fx)
}

func recountJoined(st holderState, ev holderEvent) (holderState, []holderEffect, error) {
	joined := st.joined
	st.joined = nil
	st, fx, err := settled(st, answerAll(st.waiters, ev.outcome, ev.err))
	if err != nil {
		return st, nil, err
	}
	st.waiters = nil
	for _, j := range joined {
		var more []holderEffect
		if st, more, err = releasing(st, j); err != nil {
			return st, nil, err
		}
		fx = append(fx, more...)
	}
	return st, fx, nil
}

func failedAttempt(st holderState, ev holderEvent) (holderState, []holderEffect, error) {
	fx := answerAll(st.waiters, ev.outcome, ev.err)
	st.waiters = nil
	var silent []holderEvent
	for _, j := range st.joined {
		if j.done != nil {
			fx = append(fx, answer(j.done, ev.outcome, ev.err)...)
			continue
		}
		silent = append(silent, j)
	}
	st.joined = silent
	st.retries++
	if st.retrying() && st.retries <= holderMaxRetries {
		return st, append(fx, holderEffect{kind: fxHolderRetry, n: st.retries}), nil
	}
	st.retries, st.joined = 0, nil
	return settled(st, fx)
}

func (st holderState) retrying() bool {
	return st.silent || slices.ContainsFunc(st.joined, func(j holderEvent) bool { return j.done == nil })
}

func foldJoined(st holderState) holderState {
	for _, j := range st.joined {
		if j.done != nil {
			st.waiters = append(slices.Clone(st.waiters), j.done)
		} else {
			st.silent = true
		}
		st.call.slot = st.call.slot || j.slot
	}
	st.joined = nil
	return st
}

func settled(st holderState, fx []holderEffect) (holderState, []holderEffect, error) {
	maps.Copy(st.holds, st.back)
	clear(st.back)
	st.silent = false
	next, err := st.settle()
	return next, fx, err
}

func deleted(st holderState, ev holderEvent) (holderState, []holderEffect, error) {
	fx := answerAll(st.waiters, releaseRemoved, nil)
	for _, g := range sortedHolds(st.back) {
		fx = append(fx, holderEffect{kind: fxHolderGone, grab: g})
	}
	if st.call.slot {
		fx = append(fx, holderEffect{kind: fxHolderSlot})
	}
	fx = append(fx, holderEffect{kind: fxHolderForget})
	st.waiters, st.retries, st.goneAt, st.silent = nil, 0, ev.at, false
	clear(st.holds)
	clear(st.back)
	next, err := st.moveTo(holderGone)
	return next, fx, err
}

func answerAll(waiters []chan releaseResult, outcome releaseOutcome, err error) []holderEffect {
	var fx []holderEffect
	for _, ch := range waiters {
		fx = append(fx, answer(ch, outcome, err)...)
	}
	return fx
}

type holderRecount struct {
	held  bool
	stale []string
}

type holderDeps struct {
	logger zerolog.Logger
	// recount reports whether anything but the releasing grab still holds the torrent, and which of the holds it was
	// given are dead: a grab with no record, or whose record no longer lists the torrent.
	recount func(provider, id string, call holderCall, holds []string) (holderRecount, error)
	// remove deletes the torrent at its provider; a torrent the provider does not have is removed.
	remove func(ctx context.Context, provider, id string) error
	// refusal reports why a delete must not start; lock serialises a provider's recount and delete.
	refusal    func(ctx context.Context) error
	lock       func(provider string) (unlock func())
	slotFreed  func(provider string)
	gone       func(grab, provider, id string)
	retryDelay func(n int) time.Duration
}

// holderActor runs one keyed actor per provider torrent: the only place a provider torrent is deleted.
type holderActor struct {
	holderDeps

	runner *keyed.Runner[string, holderEvent, holderState]
}

func holderKey(provider, id string) string { return provider + "\x00" + id }

func splitHolderKey(key string) (string, string) {
	provider, id, _ := strings.Cut(key, "\x00")
	return provider, id
}

func newHolderActor(g *task.Group, deps holderDeps) *holderActor {
	a := &holderActor{holderDeps: deps}
	if a.refusal == nil {
		a.refusal = func(ctx context.Context) error { return ctx.Err() }
	}
	if a.lock == nil {
		a.lock = func(string) func() { return func() {} }
	}
	if a.slotFreed == nil {
		a.slotFreed = func(string) {}
	}
	if a.gone == nil {
		a.gone = func(string, string, string) {}
	}
	if a.retryDelay == nil {
		a.retryDelay = holderRetryDelay
	}
	a.runner = keyed.New(g, "holder", a, keyed.Options[holderEvent]{
		IdleAfter: holderIdleAfter,
		Logger:    deps.logger,
	})
	return a
}

func holderRetryDelay(n int) time.Duration {
	return min(holderRetryBase<<min(n, holderRetryDoublings), holderRetryCap)
}

func (a *holderActor) Activate(context.Context, string) holderState {
	return holderState{phase: holderIdle}
}

func (a *holderActor) Handle(
	_ context.Context,
	key string,
	st *holderState,
	ev holderEvent,
	fx *keyed.Effects[string, holderEvent],
) {
	if ev.at.IsZero() {
		ev.at = time.Now()
	}
	a.run(key, st, ev, fx)
}

func (a *holderActor) run(key string, st *holderState, ev holderEvent, fx *keyed.Effects[string, holderEvent]) {
	wasSilent := st.retrying()
	next, effects, err := holderStep(*st, ev)
	if err != nil {
		a.logger.Warn().Err(err).Str("key", key).Stringer("event", ev.kind).Msg("Holder event refused")
		return
	}
	*st = next
	provider, id := splitHolderKey(key)
	if wasSilent && ev.kind == holderDeleteDone && ev.outcome == releaseFailed &&
		!slices.ContainsFunc(effects, func(e holderEffect) bool { return e.kind == fxHolderRetry }) {
		a.logger.Warn().Err(ev.err).Str("key", key).
			Msg("Failed to delete a provider torrent that was given up on")
	}
	for _, e := range effects {
		switch e.kind {
		case fxHolderDelete:
			a.startDelete(key, st, e, fx)
		case fxHolderAnswer:
			select {
			case e.ch <- e.result:
			default:
			}
		case fxHolderGone:
			a.gone(e.grab, provider, id)
		case fxHolderRetry:
			a.logger.Warn().Str("key", key).Int("retry", e.n).Msg("Provider delete failed; trying again")
			fx.After(a.retryDelay(e.n), holderEvent{kind: holderRetry})
		case fxHolderSlot:
			a.slotFreed(provider)
		case fxHolderForget:
			fx.After(holderGoneFor, holderEvent{kind: holderForget})
		}
	}
}

func (a *holderActor) startDelete(
	key string,
	st *holderState,
	e holderEffect,
	fx *keyed.Effects[string, holderEvent],
) {
	_, err := fx.GoTracked("holder-delete", func(ctx context.Context) holderEvent {
		return a.deleteOnce(ctx, key, e.call, e.holds)
	}, func(err error) holderEvent {
		outcome := releaseFailed
		if errors.Is(err, keyed.ErrChildCancelled) {
			outcome = releaseStopped
		}
		return holderEvent{kind: holderDeleteDone, outcome: outcome, err: err}
	})
	if err != nil {
		a.run(key, st, holderEvent{kind: holderDeleteDone, outcome: releaseStopped, err: err, at: time.Now()}, fx)
	}
}

// deleteOnce is the delete child: it recounts the torrent's holders and, if none is left, deletes it.
func (a *holderActor) deleteOnce(ctx context.Context, key string, call holderCall, holds []string) holderEvent {
	provider, id := splitHolderKey(key)
	done := func(outcome releaseOutcome, err error, stale []string) holderEvent {
		return holderEvent{kind: holderDeleteDone, outcome: outcome, err: err, stale: stale, at: time.Now()}
	}
	if err := a.refusal(ctx); err != nil {
		return done(releaseStopped, err, nil)
	}
	defer a.lock(provider)()
	if err := a.refusal(ctx); err != nil {
		return done(releaseStopped, err, nil)
	}
	counted, err := a.recount(provider, id, call, holds)
	if err != nil {
		return done(releaseFailed, fmt.Errorf("recount holders: %w", err), nil)
	}
	if counted.held {
		return done(releaseHeldElsewhere, nil, counted.stale)
	}
	err = a.remove(ctx, provider, id)
	switch {
	case err == nil:
		return done(releaseRemoved, nil, counted.stale)
	case placementStopped(err):
		return done(releaseStopped, err, counted.stale)
	}
	return done(releaseFailed, err, counted.stale)
}

// providerDelete deletes one torrent at its provider on the manager's stop grace, so a delete started survives its
// owner's stop for at most placementStopGrace. A torrent the provider no longer has counts as deleted.
func (m *Manager) providerDelete(ctx context.Context, provider, id string) error {
	client := m.ProviderClient(provider)
	if client == nil {
		return errors.New("provider not configured")
	}
	delCtx := m.deleteContext(ctx)
	delErr := client.DeleteTorrent(delCtx, id)
	if delErr == nil {
		return nil
	}
	if _, getErr := client.GetTorrent(delCtx, id); errors.Is(getErr, customerror.ErrTorrentNotFound) {
		return nil
	}
	if delCtx.Err() != nil {
		cause := context.Cause(delCtx)
		m.logger.Warn().Err(cause).Str("provider", provider).Str("id", id).
			Msg("Provider torrent may be left behind after the stop")
		return fmt.Errorf("%w: %w: %w", errPlacementStopped, delErr, cause)
	}
	return delErr
}
