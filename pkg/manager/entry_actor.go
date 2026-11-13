package manager

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const entryMailboxSize = 64

// entryMsg is one message to a grab's actor: a reducer event (ev), a child's exit, or a phase retry.
type entryMsg struct {
	// ev goes to grab.Transition; nil for a child exit without a result and for a retry.
	ev *grab.Event
	// entry is the new queue record a Submit carries, and req the add request it came with.
	entry *storage.Entry
	req   *ImportRequest
	// update applies what a child saw to the record; it runs again when a failed save is retried.
	update func(e *storage.Entry)
	// torrent is what a submit or poll child saw at the provider.
	torrent  *debridTypes.Torrent
	tags     []string
	category string
	progress float64
	// providers are the providers a throttled submit waits on.
	providers []string
	// exit marks a tracked child's exit; child names it and exitErr says why it ended without a result.
	exit    bool
	child   grab.ChildKind
	exitErr error
	// retry restarts child, if the grab is still in the phase that runs it.
	retry bool
	// lib is a write to the key's library record, applied by the actor while the key has a grab.
	lib *libraryOp
}

func eventMsg(ev grab.Event) entryMsg { return entryMsg{ev: &ev} }

// placementRef names a placement the cleanup child releases at its provider.
type placementRef struct {
	Provider, TorrentID string
}

// entryChildCall is what a child of a grab is started with.
type entryChildCall struct {
	kind     grab.ChildKind
	grab     grab.Grab
	cleanup  grab.Cleanup
	releases []placementRef
	req      *ImportRequest
}

// entryState is one grab's actor state.
type entryState struct {
	loadErr  error
	g        grab.Grab
	live     map[grab.ChildKind]keyed.ChildID
	pending  []grab.Effect
	releases []placementRef
	// req is the add request of a grab submitted since the start; a grab loaded from disk has none.
	req      *ImportRequest
	timer    keyed.TimerID
	hasTimer bool
	retries  int
	// saveTimer redelivers a message whose save failed; saveFails backs it off.
	saveTimer keyed.TimerID
	hasSave   bool
	saveFails int
	// throttles counts throttled submits since the grab last reached its provider.
	throttles int
	// putAt and putProgress are when the action last saved its progress, and the progress it saved.
	putAt       time.Time
	putProgress float64
}

// entryDeps is what the entry actor needs outside the grab.
type entryDeps struct {
	store    *storage.Storage
	grabs    *grabIndex
	logger   zerolog.Logger
	capacity int
	// child builds a child's work; it must not write the queue or the library or Ask.
	child func(call entryChildCall) func(ctx context.Context) entryMsg
	// publish writes the library entry of a grab entering its action.
	publish func(e *storage.Entry) error
	// fail runs what failing a grab does outside its record.
	fail func(g grab.Grab, effect grab.FailEffect)
	// packOf maps season child keys to their pack; inLibrary reports a key the library holds.
	packOf     func() map[string]string
	inLibrary  func(key string) bool
	pollEvery  time.Duration
	retryDelay func(n int) time.Duration
	// completed runs what a finished grab triggers outside its record; it must not block.
	completed func(g grab.Grab)
}

// entryActor owns every grab: one keyed actor per entry key is the only writer of its queue record.
type entryActor struct {
	entryDeps

	runner  *keyed.Runner[string, entryMsg, entryState]
	admit   *admission
	refused atomic.Uint64
}

func newEntryActor(g *task.Group, deps entryDeps) *entryActor {
	a := &entryActor{entryDeps: deps}
	a.admit = newAdmission(deps.capacity, func(key string, ev grab.Event) error {
		return a.runner.Send(key, eventMsg(ev))
	})
	a.runner = keyed.New(g, "entry", a, keyed.Options[entryMsg]{
		MailboxSize: entryMailboxSize,
		IdleAfter:   time.Minute,
		Logger:      deps.logger,
		Class:       entryClass,
		CoalesceKey: func(m entryMsg) string { return m.ev.Kind.String() },
	})
	return a
}

// entryClass makes provider observations, progress and slot wake-ups observations; the rest are commands.
func entryClass(m entryMsg) keyed.Class {
	observations := []grab.EventKind{grab.EvProviderObserved, grab.EvActionProgress, grab.EvSlotFreed}
	if m.ev != nil && slices.Contains(observations, m.ev.Kind) {
		return keyed.Observation
	}
	return keyed.Command
}

// Activate loads key's grab. A read error refuses every command; a document that does not load is derived from
// the record's legacy fields.
func (a *entryActor) Activate(_ context.Context, key string) entryState {
	st := entryState{live: make(map[grab.ChildKind]keyed.ChildID), g: grab.Grab{State: grab.State{Key: key}}}
	e, err := a.store.GetQueuedGrab(key)
	switch {
	case errors.Is(err, appendstore.ErrKeyNotFound):
		return st
	case err != nil:
		st.loadErr = err
		return st
	}
	g, err := grab.Load(e, a.packOf(), a.inLibrary(key))
	if err != nil {
		a.logger.Warn().Err(err).Str("key", key).Msg("Grab document unreadable; using the state its record implies")
	}
	st.g = g
	a.grabs.publish(GrabView{State: g.State, Entry: *g.Entry})
	return st
}

// Handle runs one message of key's grab.
func (a *entryActor) Handle(
	_ context.Context,
	key string,
	st *entryState,
	m entryMsg,
	fx *keyed.Effects[string, entryMsg],
) {
	if st.loadErr != nil {
		fx.Reply(fmt.Errorf("grab %s unreadable: %w", key, st.loadErr))
		return
	}
	if m.lib != nil {
		a.library(key, st, m.lib, fx)
		return
	}
	if m.exit {
		m = a.childExited(key, st, m)
	}
	switch {
	case m.ev != nil:
		a.step(key, st, m, fx)
	case m.retry:
		a.retry(key, st, m.child, fx)
	}
	if m.exit {
		a.rearm(st, m, fx)
		a.startPending(key, st, fx)
	}
}

// childExited forgets the child and frees the submit's slot. A submit that ended without a result has an unknown
// outcome.
func (a *entryActor) childExited(key string, st *entryState, m entryMsg) entryMsg {
	delete(st.live, m.child)
	if m.child == grab.ChildSubmit {
		a.admit.Release(key)
		if m.ev == nil {
			m.ev = &grab.Event{Kind: grab.EvSubmitResult, Outcome: grab.OutcomeUnknown}
		}
	}
	if m.exitErr != nil {
		a.logger.Warn().Err(m.exitErr).Str("key", key).Stringer("child", childKind(m.child)).
			Msg("Grab child ended without a result")
	}
	return m
}

// rearm schedules the next run of the phase's own child once one exits: the next poll, or a retry of a child that
// gave no result.
func (a *entryActor) rearm(st *entryState, m entryMsg, fx *keyed.Effects[string, entryMsg]) {
	phase := st.g.State.Phase
	if phase == grab.PhaseProvider && m.child == grab.ChildPoll {
		a.arm(st, fx, a.pollEvery, eventMsg(grab.Event{Kind: grab.EvPollTimer}))
		return
	}
	kind, ok := phaseChild(phase)
	if !ok || kind != m.child || m.ev != nil {
		return
	}
	if _, running := st.live[kind]; running {
		return
	}
	delay := a.retryDelay(st.retries)
	if kind == grab.ChildLinks {
		delay = a.pollEvery
	}
	st.retries++
	a.arm(st, fx, delay, entryMsg{retry: true, child: kind})
}

// retry starts child again, only if the grab is still in the phase that runs it.
func (a *entryActor) retry(key string, st *entryState, child grab.ChildKind, fx *keyed.Effects[string, entryMsg]) {
	kind, ok := phaseChild(st.g.State.Phase)
	if !ok || kind != child {
		return
	}
	a.start(key, st, grab.Effect{Kind: grab.FxStartChild, Child: child, Cleanup: st.g.State.Cleanup}, false, fx)
}

// phaseChild is the child a phase retries; the poll is driven by its timer event instead.
func phaseChild(p grab.Phase) (grab.ChildKind, bool) {
	switch p {
	case grab.PhaseAwaitingLinks:
		return grab.ChildLinks, true
	case grab.PhaseActing:
		return grab.ChildAction, true
	case grab.PhaseDeleting:
		return grab.ChildCleanup, true
	case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseProvider,
		grab.PhaseComplete, grab.PhaseFailed:
	}
	return 0, false
}

// childPhase is the phase a child's work belongs to; a pending start for another phase is dropped.
func childPhase(c grab.ChildKind) grab.Phase {
	switch c {
	case grab.ChildSubmit, grab.ChildLookup:
		return grab.PhaseSubmitting
	case grab.ChildPoll:
		return grab.PhaseProvider
	case grab.ChildLinks:
		return grab.PhaseAwaitingLinks
	case grab.ChildAction:
		return grab.PhaseActing
	case grab.ChildCleanup:
		return grab.PhaseDeleting
	}
	return grab.PhaseAbsent
}

// entryStep is one accepted event being applied: the grab it leaves, the grab it makes, and what it did so far.
type entryStep struct {
	key       string
	st        *entryState
	cur       grab.Grab
	next      grab.Grab
	m         entryMsg
	ev        grab.Event
	fx        *keyed.Effects[string, entryMsg]
	published bool
	persisted bool
	changed   bool
}

// step runs one event through the reducer and applies its effects. Memory never runs ahead of disk: the new
// state replaces the old only once every save succeeded; a failed save is retried by redelivering the message.
func (a *entryActor) step(key string, st *entryState, m entryMsg, fx *keyed.Effects[string, entryMsg]) {
	ev := *m.ev
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	cur := st.g
	if ev.Kind == grab.EvActionProgress && cur.State.Phase == grab.PhaseActing && a.progressSkipped(st, m, ev.At) {
		fx.Reply(nil)
		return
	}
	if err := withSubmitted(key, &cur, ev, m); err != nil {
		fx.Reply(err)
		return
	}
	nextState, effects, err := grab.Transition(cur.State, ev)
	if err != nil {
		a.refuse(key, ev, err, fx)
		return
	}
	if ev.Kind == grab.EvSubmit && m.req != nil {
		st.req = m.req
	}
	s := &entryStep{key: key, st: st, cur: cur, next: grab.Grab{State: nextState}, m: m, ev: ev, fx: fx}
	if cur.Entry != nil {
		s.next.Entry = cur.Entry.Clone()
	}
	s.changed = applyPayload(s.next.Entry, m, cur.State.Phase)
	if nextState.Phase != cur.State.Phase {
		a.leftPhase(st, nextState.Phase, fx)
	}
	if saveErr := a.apply(s, effects); saveErr != nil {
		a.saveFailed(s, saveErr)
		return
	}
	if st.hasSave {
		fx.CancelTimer(st.saveTimer)
		st.hasSave = false
	}
	st.saveFails = 0
	st.g = s.next
	if ev.Kind == grab.EvActionProgress {
		st.putAt, st.putProgress = ev.At, m.progress
	}
	if ev.Kind == grab.EvPollTimer && nextState.Phase == grab.PhaseProvider {
		a.start(key, st, grab.Effect{Kind: grab.FxStartChild, Child: grab.ChildPoll}, false, fx)
	}
	fx.Reply(nil)
	if cur.State.Phase != grab.PhaseComplete && nextState.Phase == grab.PhaseComplete && s.next.Entry != nil {
		a.completed(s.next)
	}
	if ev.Kind == grab.EvActionDone && nextState.Phase == grab.PhaseComplete && s.next.Entry != nil &&
		s.next.Entry.Action == config.DownloadActionNone {
		a.step(key, st, eventMsg(grab.Event{Kind: grab.EvActionNone, At: ev.At}), fx)
	}
}

// withSubmitted puts the record a submit carries into cur: a new grab takes it, a failed one is re-added with it.
func withSubmitted(key string, cur *grab.Grab, ev grab.Event, m entryMsg) error {
	if ev.Kind != grab.EvSubmit {
		return nil
	}
	switch {
	case cur.State.Phase == grab.PhaseAbsent:
		if m.entry == nil {
			return fmt.Errorf("submit %s without a record", key)
		}
		cur.Entry = m.entry
		cur.State.NZB = m.entry.IsNZB()
	case cur.State.Phase == grab.PhaseFailed && m.entry != nil && cur.Entry != nil:
		cur.Entry = readded(cur.Entry, m.entry)
		cur.State.NZB = m.entry.IsNZB()
	}
	return nil
}

// readded is the record a re-add of a failed grab submits: the new request's, keeping when the grab was first
// added and the placements it had.
func readded(failed, added *storage.Entry) *storage.Entry {
	e := added.Clone()
	e.AddedOn = failed.AddedOn
	e.Providers = failed.Clone().Providers
	return e
}

// saveFailed keeps the old state and redelivers the message after a backoff, so its save is tried again.
func (a *entryActor) saveFailed(s *entryStep, err error) {
	a.logger.Error().Err(err).Str("key", s.key).Stringer("event", s.ev.Kind).
		Msg("Grab not saved; it stays as it was and the save is retried")
	s.fx.Reply(err)
	again := s.m
	again.exit, again.exitErr = false, nil
	again.ev = &s.ev
	if s.st.hasSave {
		s.fx.CancelTimer(s.st.saveTimer)
	}
	s.st.saveTimer, s.st.hasSave = s.fx.After(a.retryDelay(s.st.saveFails), again), true
	s.st.saveFails++
}

// apply applies the reducer's effects in order; on a failed save it stops and returns the error.
func (a *entryActor) apply(s *entryStep, effects []grab.Effect) error {
	for _, e := range effects {
		if err := a.effect(s, e); err != nil {
			return err
		}
	}
	if s.next.State.Phase == grab.PhaseThrottled && s.cur.State.Phase != grab.PhaseThrottled {
		s.st.throttles++
		a.admit.Throttled(s.key, s.m.providers)
		a.arm(s.st, s.fx, a.retryDelay(s.st.throttles), eventMsg(grab.Event{Kind: grab.EvThrottleTimer}))
	}
	if !s.persisted && s.changed && s.next.State.Phase != grab.PhaseDeleting {
		return a.persist(s.next)
	}
	return nil
}

// effect applies one reducer effect.
func (a *entryActor) effect(s *entryStep, e grab.Effect) error {
	st, fx := s.st, s.fx
	switch e.Kind {
	case grab.FxPersist:
		return a.save(s)
	case grab.FxRequestAdmission:
		a.admit.Request(s.key)
		a.arm(st, fx, a.retryDelay(st.retries), eventMsg(grab.Event{Kind: grab.EvAdmissionDeadline}))
		st.retries++
	case grab.FxCancelAdmission:
		a.admit.Cancel(s.key)
	case grab.FxStartChild:
		a.startWith(s.key, st, s.next, e, s.published, fx)
	case grab.FxCancelChildren:
		st.pending = nil
		for kind, id := range st.live {
			if kind != grab.ChildSubmit {
				fx.CancelAwait(id)
			}
		}
	case grab.FxReleasePlacement:
		st.releases = append(st.releases, placementRef{Provider: e.Provider, TorrentID: e.TorrentID})
	case grab.FxFail:
		a.fail(s.next, e.Fail)
	case grab.FxRemoveRecord:
		if err := a.store.DeleteQueued(s.key, nil); err != nil {
			return fmt.Errorf("remove queue record: %w", err)
		}
		a.grabs.remove(s.key)
		s.next.Entry = nil
		st.releases, st.req = nil, nil
	}
	return nil
}

// save writes the library entry first when the move calls for it, then the queue record. A grab becoming absent
// has no record to save: its removal follows.
func (a *entryActor) save(s *entryStep) error {
	if s.next.State.Phase == grab.PhaseAbsent {
		return nil
	}
	if a.publishesLibrary(s.cur, s.next, s.ev) {
		if err := a.publish(s.next.Entry); err != nil {
			return fmt.Errorf("publish library entry: %w", err)
		}
		s.published = true
	}
	if err := a.persist(s.next); err != nil {
		return err
	}
	s.persisted = true
	return nil
}

// publishesLibrary reports a save that must write the library entry first: a grab sent to a provider entering
// its action, and its action finishing.
func (a *entryActor) publishesLibrary(cur, next grab.Grab, ev grab.Event) bool {
	if next.State.PackKey != "" || next.Entry == nil {
		return false
	}
	entering := next.State.Phase == grab.PhaseActing && cur.State.Phase != grab.PhaseActing
	return entering || ev.Kind == grab.EvActionDone
}

// persist saves g's queue record and shows it to readers.
func (a *entryActor) persist(g grab.Grab) error {
	e := g.Entry.Clone()
	if err := grab.Encode(grab.Grab{State: g.State, Entry: e}); err != nil {
		return fmt.Errorf("encode grab: %w", err)
	}
	if err := a.store.UpdateQueuedGrab(e); err != nil {
		return err
	}
	*g.Entry = *e
	a.grabs.publish(GrabView{State: g.State, Entry: *e})
	return nil
}

// refuse counts and logs an event the grab's phase does not take; a refused admission returns its slot.
func (a *entryActor) refuse(key string, ev grab.Event, err error, fx *keyed.Effects[string, entryMsg]) {
	a.refused.Add(1)
	a.logger.Debug().Err(err).Str("key", key).Msg("Grab event refused")
	if ev.Kind == grab.EvAdmitted {
		a.admit.Release(key)
	}
	fx.Reply(err)
}

// leftPhase drops the timer and retry count of the phase the grab leaves for to, before to's effects arm their own.
func (a *entryActor) leftPhase(st *entryState, to grab.Phase, fx *keyed.Effects[string, entryMsg]) {
	if st.hasTimer {
		fx.CancelTimer(st.timer)
		st.hasTimer = false
	}
	st.retries = 0
	st.putAt = time.Time{}
	switch to {
	case grab.PhaseProvider, grab.PhaseAwaitingLinks, grab.PhaseActing:
		st.throttles = 0
	case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseComplete,
		grab.PhaseFailed, grab.PhaseDeleting:
	}
}

// arm replaces the grab's one phase timer.
func (a *entryActor) arm(st *entryState, fx *keyed.Effects[string, entryMsg], d time.Duration, m entryMsg) {
	if st.hasTimer {
		fx.CancelTimer(st.timer)
	}
	st.timer, st.hasTimer = fx.After(d, m), true
}

// startWith starts a child the reducer asked for, from the state being entered.
func (a *entryActor) startWith(
	key string,
	st *entryState,
	g grab.Grab,
	e grab.Effect,
	published bool,
	fx *keyed.Effects[string, entryMsg],
) {
	saved := st.g
	st.g = g
	a.start(key, st, e, published, fx)
	st.g = saved
}

// start runs a child unless its kind is running; a child that must follow another waits in pending, and a
// cleanup waits for every child. A grab sent to a provider has its library entry written before its action.
func (a *entryActor) start(
	key string,
	st *entryState,
	e grab.Effect,
	published bool,
	fx *keyed.Effects[string, entryMsg],
) {
	if a.mustWait(st, e) {
		st.pending = append(st.pending, e)
		return
	}
	if _, running := st.live[e.Child]; running {
		a.refused.Add(1)
		a.logger.Debug().Str("key", key).Stringer("child", childKind(e.Child)).Msg("Grab child already running")
		return
	}
	g := st.g
	if e.Child == grab.ChildAction && g.State.PackKey == "" && !published && g.Entry != nil {
		if err := a.publish(g.Entry); err != nil {
			a.logger.Error().Err(err).Str("key", key).Msg("Library entry not written; action retried")
			st.retries++
			a.arm(st, fx, a.retryDelay(st.retries), entryMsg{retry: true, child: grab.ChildAction})
			return
		}
	}
	if g.Entry != nil {
		g.Entry = g.Entry.Clone()
	}
	call := entryChildCall{
		kind: e.Child, grab: g, cleanup: e.Cleanup, releases: append([]placementRef(nil), st.releases...),
		req: st.req,
	}
	work := a.child(call)
	kind := e.Child
	id, err := fx.GoTracked("grab-"+childKind(kind).String(), func(ctx context.Context) entryMsg {
		m := work(ctx)
		m.exit, m.child = true, kind
		return m
	}, func(err error) entryMsg {
		return entryMsg{exit: true, child: kind, exitErr: err}
	})
	if err != nil {
		a.logger.Debug().Err(err).Str("key", key).Msg("Grab child not started")
		return
	}
	st.live[kind] = id
}

// mustWait reports a start that waits for another child: a cleanup for every child, a HasAfter for its kind.
func (a *entryActor) mustWait(st *entryState, e grab.Effect) bool {
	if e.Child == grab.ChildCleanup {
		return len(st.live) > 0
	}
	if e.HasAfter {
		_, running := st.live[e.AfterExit]
		return running
	}
	return false
}

// startPending starts the pending children that no longer wait, dropping those of a phase the grab left.
func (a *entryActor) startPending(key string, st *entryState, fx *keyed.Effects[string, entryMsg]) {
	waiting := st.pending
	st.pending = nil
	for _, e := range waiting {
		if _, running := st.live[e.Child]; running || childPhase(e.Child) != st.g.State.Phase {
			continue
		}
		if a.mustWait(st, e) {
			st.pending = append(st.pending, e)
			continue
		}
		a.start(key, st, e, false, fx)
	}
}

// applyPayload copies what a message carries into the record and reports whether it changed anything.
func applyPayload(e *storage.Entry, m entryMsg, phase grab.Phase) bool {
	if e == nil {
		return false
	}
	changed := false
	if m.torrent != nil {
		applyDebridTorrentToEntry(e, m.torrent)
		changed = true
	}
	if m.update != nil {
		m.update(e)
		changed = true
	}
	if phase == grab.PhaseDeleting || m.ev == nil {
		return changed
	}
	if m.ev.Kind == grab.EvTagsChanged {
		e.Tags = m.tags
		changed = true
	}
	if m.ev.Kind == grab.EvCategoryChanged {
		e.Category = m.category
		changed = true
	}
	if m.ev.Kind == grab.EvActionProgress && m.progress != e.Progress {
		e.Progress = m.progress
		changed = true
	}
	return changed
}

const (
	progressPutEvery = 5 * time.Second
	progressPutStep  = 0.01
)

// progressSkipped reports a progress report too soon and too small after the last one saved to be worth a write.
func (a *entryActor) progressSkipped(st *entryState, m entryMsg, now time.Time) bool {
	if st.putAt.IsZero() {
		return false
	}
	return now.Sub(st.putAt) < progressPutEvery && math.Abs(m.progress-st.putProgress) < progressPutStep
}

// childKind names a child kind in logs.
type childKind grab.ChildKind

func (c childKind) String() string {
	return [...]string{"submit", "lookup", "poll", "links", "action", "cleanup"}[c]
}
