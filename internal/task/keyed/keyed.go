// Package keyed runs one actor per key.
//
// An actor is a goroutine that owns the state for its key. Messages for a key
// are handled one at a time, in the order they were accepted, by that key's
// goroutine, which is a task of the runner's group. The goroutine starts with
// the first message, exits after a quiet period, and starts again with the
// next one. The state of a key, S, belongs to the runner: the handler's
// Activate builds it when the goroutine starts, every Handle call receives it
// by pointer, and the runner drops it when the goroutine exits. Anything that
// must outlast the goroutine is written to a store before Handle returns, and
// read again by the next Activate.
//
// A handler must not block on I/O. Slow work runs as a child task
// ([Effects.Go]) bound to the runner's context, and reports back with a
// message. Actors talk to each other with fire-and-forget [Effects.Send]. A
// caller outside the handlers can [Runner.Ask] for a reply with a timeout; an
// Ask from inside a handler is refused, because the handler would wait for
// itself.
//
// Mailboxes are bounded. What happens when one is full depends on the class
// of the message ([Class]): a command pushes out the oldest queued
// observation, and is refused only if there is none, so the caller can try
// again later; an observation is coalesced with an older one, or replaces the
// oldest. A timer's message and a child task's result are never dropped or
// refused for a full mailbox; they go only with a cancel (of the timer or the
// child), a stop, or an Activate that panics, which drops everything queued
// for the key and logs it. Timers belong to the runner, not to the goroutine,
// so a goroutine's idle exit never loses one.
//
// The runner's lifecycle is an explicit state machine (running, stopping,
// stopped) driven by its group: stopping the group refuses new messages,
// cancels the timers and lets each goroutine finish the message it is
// handling. Messages still queued are dropped; the callers of an Ask among
// them are told the outcome is unknown.
package keyed

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
)

// ErrStopped is returned for a message sent to a runner that is stopping or
// stopped. It is the task package's, so one [errors.Is] covers both.
var ErrStopped = task.ErrStopped

// ErrMailboxFull is returned for a command that finds its key's mailbox full.
var ErrMailboxFull = errors.New("keyed: mailbox full")

const (
	// defaultMailboxSize is the bound of a key's mailbox when Options sets none.
	defaultMailboxSize = 64
	// defaultIdleAfter is how long a key's goroutine waits for a message before it exits.
	defaultIdleAfter = time.Minute
)

// Handler handles the messages of a runner. S is the state of one key: the
// runner holds it for as long as the key's goroutine runs, hands it to every
// Handle call by pointer, and drops it when the goroutine exits. A handler
// without state uses struct{}.
type Handler[K comparable, M, S any] interface {
	// Activate builds the state of key. It runs on the key's goroutine when
	// that starts, before the first message, and may read the key's record
	// from a store. A load that fails is a state value ("load failed"), not
	// an error: the next Handle call decides what to do about it.
	Activate(ctx context.Context, key K) S
	// Handle processes one message for key and may change its state. It runs
	// on the key's goroutine, one message at a time. It must not block on
	// I/O: slow work runs as a child task (fx.Go) and reports back with a
	// message. ctx ends when the runner stops. Use only the ctx passed to
	// Handle (or one derived from it): it marks the call as a handler's, which
	// is how Ask refuses to run from inside one.
	Handle(ctx context.Context, key K, st *S, msg M, fx *Effects[K, M])
}

// HandlerFunc makes a stateless Handler of a function.
type HandlerFunc[K comparable, M any] func(ctx context.Context, key K, msg M, fx *Effects[K, M])

// Activate has no state to build.
func (HandlerFunc[K, M]) Activate(context.Context, K) struct{} { return struct{}{} }

// Handle calls f.
func (f HandlerFunc[K, M]) Handle(ctx context.Context, key K, _ *struct{}, msg M, fx *Effects[K, M]) {
	f(ctx, key, msg, fx)
}

// host is what Effects needs of the runner it belongs to, without the runner's state type.
type host[K comparable, M any] interface {
	Send(key K, msg M) error
	After(key K, d time.Duration, msg M) TimerID
	CancelTimer(id TimerID)
	spawn(key K, name string, fn func(ctx context.Context) M, onPanic func(v any) M) (ChildID, error)
	cancelChild(id ChildID)
}

// activate builds the key's state. A panic is logged and counted and reported
// as false: the zero state would claim things the store never said (that
// nothing holds a torrent, say), so no message is handled on it.
func (r *Runner[K, M, S]) activate(ctx context.Context, key K) (S, bool) {
	var st S
	ok := true
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				ok = false
				r.totalPanics.Add(1)
				r.logger.Error().Str("runner", r.name).Str("key", fmt.Sprint(key)).Interface("panic", recovered).
					Bytes("stack", debug.Stack()).Msg("Activate panicked; the messages waiting for the key are dropped")
			}
		}()
		st = r.handler.Activate(ctx, key)
	}()
	return st, ok
}

// abandonLocked ends the goroutine of a key whose Activate panicked: the messages queued so far are dropped and
// counted, an Ask among them is answered with ErrHandlerPanicked, and the next message starts the key again.
// Timer and child messages are dropped too, since there is no state to handle them on: the warning names how many,
// because a dropped deadline is not handled until the key's next Activate sets it again.
func (r *Runner[K, M, S]) abandonLocked(a *actor[K, M]) {
	timers, children := 0, 0
	for _, env := range a.queue {
		if env.reply != nil {
			env.reply.finish(nil, ErrHandlerPanicked)
		}
		switch {
		case env.timer != 0:
			timers++
		case env.child != 0:
			children++
		}
		r.forgetQueuedLocked(env)
		a.dropped++
		r.totalDropped.Add(1)
	}
	if timers > 0 || children > 0 {
		r.logger.Warn().Str("runner", r.name).Str("key", fmt.Sprint(a.key)).Int("timers", timers).
			Int("child_results", children).Msg("Activate panicked; timer messages and child results were dropped")
	}
	a.queue, a.bypassed = nil, 0
	r.exitLocked(a, false)
}

// forgetQueuedLocked forgets that env waits in a mailbox, once it leaves the queue: handled, dropped or recalled.
func (r *Runner[K, M, S]) forgetQueuedLocked(env envelope[M]) {
	delete(r.queuedTimers, env.timer)
	delete(r.queuedChildren, env.child)
}

// Options sets how a runner behaves. The zero value is usable.
type Options[M any] struct {
	// MailboxSize bounds the queued commands and observations of one key. The default is 64.
	MailboxSize int
	// IdleAfter is how long a key's goroutine waits with an empty mailbox before it exits. The default is a
	// minute; a negative value means never. A runner made by NewSingleton never exits.
	IdleAfter time.Duration
	// Logger receives the runner's reports, such as a handler panic. The zero value logs nothing.
	Logger zerolog.Logger
	// Class tells what a message is when it is sent; nil makes every message a Command.
	Class func(M) Class
	// CoalesceKey names the observation a message replaces: an observation queued behind another with the same
	// non-empty key replaces it, in its place in the queue. It is called only for observations.
	CoalesceKey func(M) string
}

// runState is the lifecycle of a runner.
type runState uint32

const (
	// runRunning accepts messages.
	runRunning runState = iota + 1
	// runStopping refuses messages and waits for the handlers in flight.
	runStopping
	// runStopped has no goroutine left.
	runStopped
)

func (s runState) String() string {
	switch s {
	case runRunning:
		return "running"
	case runStopping:
		return "stopping"
	case runStopped:
		return "stopped"
	default:
		return fmt.Sprintf("runState(%d)", uint32(s))
	}
}

// runSuccessors is the transition table of the runner.
func runSuccessors(s runState) []runState {
	switch s {
	case runRunning:
		return []runState{runStopping}
	case runStopping:
		return []runState{runStopped}
	case runStopped:
		return nil
	default:
		return nil
	}
}

// envelope is a queued message.
type envelope[M any] struct {
	msg   M
	at    time.Time
	class Class
	// coalesce is the key an observation coalesces on, or empty.
	coalesce string
	// bypass marks a message that does not count against the mailbox bound: a timer, or a child task's result.
	bypass bool
	// timer is the timer that delivered the message, so cancelling the timer can take the message back.
	timer TimerID
	// child is the child task that sent the message as its result, so cancelling the child can take it back.
	child ChildID
	// reply is where the asker of an Ask waits; nil for a message from Send.
	reply *replySlot
}

// Class says what a message is, which decides what happens to it when its
// key's mailbox is full.
type Class uint8

const (
	// Command is a request that must not be lost silently: into a full mailbox it pushes out the oldest queued
	// observation, and is refused with ErrMailboxFull, so the caller can try again later, only if there is none.
	Command Class = iota + 1
	// Observation is a report of how things stand: the latest replaces an older one with the same coalesce key,
	// and into a full mailbox the oldest observation is dropped.
	Observation
)

// actor is the runner's record of one key. The runner's mutex guards every field.
type actor[K comparable, M any] struct {
	key                         K
	queue                       []envelope[M]
	bypassed                    int // envelopes in queue that bypass the bound
	running                     bool
	wake                        chan struct{}
	children                    int // child tasks in flight
	timers                      int // timers pending
	handled                     uint64
	coalesced, dropped, refused uint64
	panics                      uint64
}

func (a *actor[K, M]) depth() int { return len(a.queue) - a.bypassed }

// Runner runs one actor per key.
type Runner[K comparable, M, S any] struct {
	name      string
	group     *task.Group
	handler   Handler[K, M, S]
	opts      Options[M]
	logger    zerolog.Logger
	noIdle    bool
	lifecycle *state.Atomic[runState]
	ctx       context.Context

	mu   sync.Mutex
	keys map[K]*actor[K, M]
	live int // key goroutines running or starting
	// childRecs holds the child tasks running, so its length is their count.
	childRecs map[ChildID]*childRec[K]
	// queuedChildren maps a finished child whose result still waits in a mailbox to its key.
	queuedChildren map[ChildID]K
	lastChild      ChildID

	// Timers belong to the runner (see timers.go). Guarded by mu.
	timers    timerHeap[K, M]
	timerByID map[TimerID]*timerEntry[K, M]
	// queuedTimers maps a fired timer whose message still waits in a mailbox to its key.
	queuedTimers map[TimerID]K
	lastTimer    TimerID
	timerWake    chan struct{}

	totalHandled                               atomic.Uint64
	totalPanics                                atomic.Uint64
	totalChildPanics                           atomic.Uint64
	totalCoalesced, totalDropped, totalRefused atomic.Uint64
}

// New makes a runner whose key goroutines are tasks of a child of g named
// name. Stopping g stops the runner. A runner made on a group that is not
// running refuses every message.
func New[K comparable, M, S any](g *task.Group, name string, h Handler[K, M, S], opts Options[M]) *Runner[K, M, S] {
	if opts.MailboxSize <= 0 {
		opts.MailboxSize = defaultMailboxSize
	}
	if opts.IdleAfter == 0 {
		opts.IdleAfter = defaultIdleAfter
	}
	child := g.Child(name)
	r := &Runner[K, M, S]{
		name:           name,
		group:          child,
		handler:        h,
		opts:           opts,
		logger:         opts.Logger,
		noIdle:         opts.IdleAfter < 0,
		lifecycle:      state.NewAtomic(runRunning, runSuccessors),
		ctx:            child.Context(),
		keys:           make(map[K]*actor[K, M]),
		timerByID:      make(map[TimerID]*timerEntry[K, M]),
		queuedTimers:   make(map[TimerID]K),
		childRecs:      make(map[ChildID]*childRec[K]),
		queuedChildren: make(map[ChildID]K),
		timerWake:      make(chan struct{}, 1),
	}
	if err := child.Go("supervisor", r.supervise); err != nil {
		r.lifecycle.Transition(runRunning, runStopping)
		r.lifecycle.Transition(runStopping, runStopped)
	}
	return r
}

// Singleton is a runner with the one key and no idle exit: its goroutine, and
// so its state, lives until the runner stops. It is for the services that are
// one thing each (a mount supervisor, a run coordinator, a configuration
// hot-apply).
type Singleton[M, S any] struct {
	r *Runner[struct{}, M, S]
}

// NewSingleton starts a singleton on a child of g named name.
func NewSingleton[M, S any](
	g *task.Group,
	name string,
	h Handler[struct{}, M, S],
	opts Options[M],
) *Singleton[M, S] {
	opts.IdleAfter = -1
	r := New(g, name, h, opts)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lifecycle.Load() == runRunning {
		a := r.actorLocked(struct{}{})
		if err := r.startLocked(a); err != nil {
			r.logger.Warn().Err(err).Str("runner", name).Msg("Singleton not started")
		}
	}
	return &Singleton[M, S]{r: r}
}

// Send hands msg to the singleton without waiting. See [Runner.Send].
func (s *Singleton[M, S]) Send(msg M) error { return s.r.Send(struct{}{}, msg) }

// Ask sends msg and waits for the reply. See [Runner.Ask].
func (s *Singleton[M, S]) Ask(ctx context.Context, msg M) (any, error) {
	return s.r.Ask(ctx, struct{}{}, msg)
}

// After delivers msg to the singleton once d has passed. See [Runner.After].
func (s *Singleton[M, S]) After(d time.Duration, msg M) TimerID { return s.r.After(struct{}{}, d, msg) }

// CancelTimer cancels a pending timer. See [Runner.CancelTimer].
func (s *Singleton[M, S]) CancelTimer(id TimerID) { s.r.CancelTimer(id) }

// Snapshot describes the singleton's runner. See [Runner.Snapshot].
func (s *Singleton[M, S]) Snapshot() Info { return s.r.Snapshot() }

// beginStop moves the runner to Stopping, and on to Stopped once no key
// goroutine is left.
func (r *Runner[K, M, S]) beginStop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lifecycle.Transition(runRunning, runStopping)
	r.clearTimersLocked()
	r.settleLocked()
}

// settleLocked moves a stopping runner with no key goroutine and no child task left to Stopped.
func (r *Runner[K, M, S]) settleLocked() {
	if r.live == 0 && len(r.childRecs) == 0 {
		r.lifecycle.Transition(runStopping, runStopped)
	}
}

// accepting reports whether the runner takes messages. The group's context
// ends before the supervisor task has marked the runner as stopping, so both
// are checked.
func (r *Runner[K, M, S]) accepting() bool {
	return r.lifecycle.Load() == runRunning && r.ctx.Err() == nil
}

// Send hands msg to key's actor without waiting for it. It returns ErrStopped
// once the runner stops and ErrMailboxFull when the key's mailbox has no room
// for a message of its class.
func (r *Runner[K, M, S]) Send(key K, msg M) error {
	env := envelope[M]{msg: msg, at: time.Now(), class: Command}
	if r.opts.Class != nil {
		if class := r.opts.Class(msg); class == Observation {
			env.class = class
		}
	}
	if env.class == Observation && r.opts.CoalesceKey != nil {
		env.coalesce = r.opts.CoalesceKey(msg)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enqueueLocked(key, env)
}

// admitLocked applies the overflow policy of env's class to a's mailbox. It reports whether env is to be
// queued; a nil error with false means env was coalesced into a queued message.
func (r *Runner[K, M, S]) admitLocked(a *actor[K, M], env envelope[M]) (bool, error) {
	switch {
	case env.bypass:
		return true, nil
	case env.class == Observation:
		if env.coalesce != "" {
			i := slices.IndexFunc(a.queue, func(queued envelope[M]) bool {
				return !queued.bypass && queued.class == Observation && queued.coalesce == env.coalesce
			})
			if i >= 0 {
				a.queue[i].msg = env.msg
				a.coalesced++
				r.totalCoalesced.Add(1)
				return false, nil
			}
		}
		if a.depth() < r.opts.MailboxSize {
			return true, nil
		}
		if !r.evictObservationLocked(a) {
			return false, r.refuseLocked(a)
		}
		return true, nil
	default:
		// A command, or an Ask, pushes out the oldest queued observation rather than be refused.
		if a.depth() >= r.opts.MailboxSize && !r.evictObservationLocked(a) {
			return false, r.refuseLocked(a)
		}
		return true, nil
	}
}

// evictObservationLocked drops the oldest queued observation, counting it, and reports whether there was one.
// Messages that bypass the bound are never dropped.
func (r *Runner[K, M, S]) evictObservationLocked(a *actor[K, M]) bool {
	oldest := slices.IndexFunc(a.queue, func(queued envelope[M]) bool {
		return !queued.bypass && queued.class == Observation
	})
	if oldest < 0 {
		return false
	}
	a.queue = slices.Delete(a.queue, oldest, oldest+1)
	a.dropped++
	r.totalDropped.Add(1)
	return true
}

func (r *Runner[K, M, S]) refuseLocked(a *actor[K, M]) error {
	a.refused++
	r.totalRefused.Add(1)
	r.dropIfIdleLocked(a)
	return ErrMailboxFull
}

// enqueueLocked queues env for key and starts the key's goroutine if it is not running.
func (r *Runner[K, M, S]) enqueueLocked(key K, env envelope[M]) error {
	if !r.accepting() {
		return ErrStopped
	}
	a := r.actorLocked(key)
	queued, err := r.admitLocked(a, env)
	if err != nil || !queued {
		return err
	}
	a.queue = append(a.queue, env)
	if env.bypass {
		a.bypassed++
	}
	if a.running {
		select {
		case a.wake <- struct{}{}:
		default:
		}
		return nil
	}
	if startErr := r.startLocked(a); startErr != nil {
		a.queue = a.queue[:len(a.queue)-1]
		if env.bypass {
			a.bypassed--
		}
		r.dropIfIdleLocked(a)
		return startErr
	}
	return nil
}

func (r *Runner[K, M, S]) actorLocked(key K) *actor[K, M] {
	a, ok := r.keys[key]
	if !ok {
		a = &actor[K, M]{key: key, wake: make(chan struct{}, 1)}
		r.keys[key] = a
	}
	return a
}

// dropIfIdleLocked forgets a key that has nothing queued, nothing running and nothing in flight. A singleton's
// record stays while the runner runs (its goroutine never exits); once the runner stops it goes like any other.
func (r *Runner[K, M, S]) dropIfIdleLocked(a *actor[K, M]) {
	if !a.running && len(a.queue) == 0 && a.children == 0 && a.timers == 0 && (!r.noIdle || !r.accepting()) {
		delete(r.keys, a.key)
	}
}

// startLocked starts the key's goroutine.
func (r *Runner[K, M, S]) startLocked(a *actor[K, M]) error {
	a.running = true
	r.live++
	err := r.group.Go(fmt.Sprint(a.key), func(ctx context.Context) error {
		r.loop(ctx, a)
		return nil
	})
	if err != nil {
		a.running = false
		r.live--
		r.settleLocked()
		return ErrStopped
	}
	return nil
}

// loop is the key's goroutine: it handles queued messages in order and exits
// when the runner stops or the key has been quiet for IdleAfter.
func (r *Runner[K, M, S]) loop(ctx context.Context, a *actor[K, M]) {
	hctx := withHandlerMarker(ctx)
	held, ok := r.activate(hctx, a.key)
	if !ok {
		r.mu.Lock()
		r.abandonLocked(a)
		r.mu.Unlock()
		return
	}
	var idle *time.Timer
	if !r.noIdle {
		idle = time.NewTimer(r.opts.IdleAfter)
		defer idle.Stop()
	}
	for {
		r.mu.Lock()
		if !r.accepting() {
			r.exitLocked(a, true)
			r.mu.Unlock()
			return
		}
		if len(a.queue) > 0 {
			env := a.queue[0]
			a.queue[0] = envelope[M]{}
			a.queue = a.queue[1:]
			if env.bypass {
				a.bypassed--
			}
			r.forgetQueuedLocked(env)
			r.mu.Unlock()
			r.handle(hctx, a, &held, env)
			continue
		}
		r.mu.Unlock()

		var idleC <-chan time.Time
		if idle != nil {
			idle.Reset(r.opts.IdleAfter)
			idleC = idle.C
		}
		select {
		case <-a.wake:
		case <-ctx.Done():
		case <-idleC:
			r.mu.Lock()
			// A child task in flight will send its result to this key: the goroutine stays up for it. Pending
			// timers do not hold it; the key's record does that, and a firing timer starts the goroutine again.
			if len(a.queue) == 0 && a.children == 0 {
				r.exitLocked(a, false)
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
		}
	}
}

// exitLocked ends the key's goroutine. On a stop the queue is dropped.
func (r *Runner[K, M, S]) exitLocked(a *actor[K, M], stopping bool) {
	if stopping {
		for _, env := range a.queue {
			dropReply(env.reply)
			r.forgetQueuedLocked(env)
		}
		a.queue, a.bypassed = nil, 0
	}
	a.running = false
	r.live--
	r.dropIfIdleLocked(a)
	if !r.accepting() {
		r.settleLocked()
	}
}

// handle runs the handler for one message, recovering a panic.
func (r *Runner[K, M, S]) handle(ctx context.Context, a *actor[K, M], st *S, env envelope[M]) {
	fx := &Effects[K, M]{r: r, key: a.key}
	defer func() {
		recovered := recover()
		fx.done.Store(true)
		fx.finishReply(env.reply, recovered != nil)
		r.mu.Lock()
		a.handled++
		if recovered != nil {
			a.panics++
		}
		r.mu.Unlock()
		r.totalHandled.Add(1)
		if recovered != nil {
			r.totalPanics.Add(1)
			r.logger.Error().Str("runner", r.name).Str("key", fmt.Sprint(a.key)).Interface("panic", recovered).
				Bytes("stack", debug.Stack()).Msg("Handler panicked; the actor goes on with its next message")
		}
	}()
	r.handler.Handle(ctx, a.key, st, env.msg, fx)
}

// Effects is what a handler can do besides change its own state. It is valid
// only inside the Handle call it was passed to.
type Effects[K comparable, M any] struct {
	r   host[K, M]
	key K

	replied bool
	value   any
	// done is set when Handle returns: Effects kept past that point would race with the actor's next message.
	done atomic.Bool
}

// live panics if Handle has already returned.
func (fx *Effects[K, M]) live() {
	if fx.done.Load() {
		panic("keyed: Effects used after Handle returned; it is valid only inside the Handle call it was passed to")
	}
}

// ChildID names a child task started with [Effects.Go].
type ChildID uint64

// childRec is the runner's record of a running child task. The runner's mutex guards it.
type childRec[K comparable] struct {
	key       K
	cancel    context.CancelFunc // set when the task starts
	cancelled bool
}

// Go runs fn as a child task of the key's actor, on a context derived from the
// runner's, and sends the message it returns back to the same key. The message
// is queued past the mailbox bound: the actor asked for it, so it is never
// refused. The returned id names the child for [Effects.CancelChild].
//
// If fn panics the runner recovers it, logs it with the key, counts it, and
// sends onPanic(v), v being the panic value, in fn's place. An actor that
// waits for the child must never wait for ever, so onPanic should say that
// the work failed; with a nil onPanic nothing is sent. Go returns ErrStopped
// once the runner stops.
func (fx *Effects[K, M]) Go(name string, fn func(ctx context.Context) M, onPanic func(v any) M) (ChildID, error) {
	fx.live()
	return fx.r.spawn(fx.key, name, fn, onPanic)
}

// CancelChild cancels a child task: its context ends, it no longer keeps the
// key's goroutine up, and whatever it returns, or its onPanic message, is
// dropped, even when it is already queued behind the message being handled.
// A child that ignores its context keeps running until it returns, but nothing
// it does reaches the actor. A child cancelled before its task starts never
// runs fn. Cancelling a child whose result has been handled, or an unknown
// one, changes nothing.
func (fx *Effects[K, M]) CancelChild(id ChildID) {
	fx.live()
	fx.r.cancelChild(id)
}

// spawn starts a child task of key's actor.
func (r *Runner[K, M, S]) spawn(
	key K,
	name string,
	fn func(ctx context.Context) M,
	onPanic func(v any) M,
) (ChildID, error) {
	r.mu.Lock()
	if !r.accepting() {
		r.mu.Unlock()
		return 0, ErrStopped
	}
	a := r.actorLocked(key)
	a.children++
	r.lastChild++
	id := r.lastChild
	rec := &childRec[K]{key: key}
	r.childRecs[id] = rec
	r.mu.Unlock()

	err := r.group.Go(fmt.Sprint(key)+"/"+name, func(ctx context.Context) error {
		defer r.childDone(key, id, rec)
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		r.mu.Lock()
		rec.cancel = cancel
		cancelled := rec.cancelled
		r.mu.Unlock()
		if cancelled {
			return nil // cancelled before it started: fn never runs
		}
		msg, ok := r.runChild(withHandlerMarker(cctx), key, name, fn, onPanic)
		if !ok {
			return nil
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if rec.cancelled {
			return nil // the actor gave up on this child: its result is dropped
		}
		env := envelope[M]{msg: msg, at: time.Now(), bypass: true, child: id}
		if deliverErr := r.enqueueLocked(key, env); deliverErr != nil {
			r.logger.Debug().Err(deliverErr).Str("runner", r.name).Str("key", fmt.Sprint(key)).
				Str("task", name).Msg("A child task's result was not delivered")
			return nil
		}
		r.queuedChildren[id] = key
		return nil
	})
	if err != nil {
		r.childDone(key, id, rec)
		return 0, ErrStopped
	}
	return id, nil
}

func (r *Runner[K, M, S]) cancelChild(id ChildID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recallChildResultLocked(id)
	rec, ok := r.childRecs[id]
	if !ok || rec.cancelled {
		return
	}
	rec.cancelled = true
	if a, found := r.keys[rec.key]; found {
		a.children--
		r.dropIfIdleLocked(a)
	}
	if rec.cancel != nil {
		rec.cancel()
	}
}

// recallChildResultLocked removes the result of a finished child from its mailbox, if it still waits there.
func (r *Runner[K, M, S]) recallChildResultLocked(id ChildID) {
	key, ok := r.queuedChildren[id]
	if !ok {
		return
	}
	delete(r.queuedChildren, id)
	a, found := r.keys[key]
	if !found {
		return
	}
	if i := slices.IndexFunc(a.queue, func(env envelope[M]) bool { return env.child == id }); i >= 0 {
		a.queue = slices.Delete(a.queue, i, i+1)
		a.bypassed--
	}
	r.dropIfIdleLocked(a)
}

// runChild runs a child task's function and returns the message to deliver. A panic in fn becomes onPanic's
// message; a panic in onPanic, or a nil onPanic, delivers nothing.
func (r *Runner[K, M, S]) runChild(
	ctx context.Context,
	key K,
	name string,
	fn func(ctx context.Context) M,
	onPanic func(v any) M,
) (M, bool) {
	var (
		msg      M
		panicked bool
		value    any
	)
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicked, value = true, recovered
			}
		}()
		msg = fn(ctx)
	}()
	if !panicked {
		return msg, true
	}
	r.totalChildPanics.Add(1)
	r.logger.Error().Str("runner", r.name).Str("key", fmt.Sprint(key)).Str("task", name).Interface("panic", value).
		Bytes("stack", debug.Stack()).Msg("Child task panicked")
	if onPanic == nil {
		var zero M
		return zero, false
	}
	var fallback M
	delivered := true
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				delivered = false
				r.logger.Error().Str("runner", r.name).Str("key", fmt.Sprint(key)).Str("task", name).
					Interface("panic", recovered).Msg("onPanic panicked; nothing is delivered for the child task")
			}
		}()
		fallback = onPanic(value)
	}()
	return fallback, delivered
}

func (r *Runner[K, M, S]) childDone(key K, id ChildID, rec *childRec[K]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.childRecs, id)
	if a, ok := r.keys[key]; ok && !rec.cancelled { // a cancelled child was released from the key already
		a.children--
		r.dropIfIdleLocked(a)
	}
	if !r.accepting() {
		r.settleLocked()
	}
}

// Send hands msg to another key of the same runner, without waiting for it.
func (fx *Effects[K, M]) Send(key K, msg M) error {
	fx.live()
	return fx.r.Send(key, msg)
}

// inHandler marks the contexts of handlers and their child tasks.
type inHandler struct{}

func withHandlerMarker(ctx context.Context) context.Context {
	return context.WithValue(ctx, inHandler{}, struct{}{})
}

// KeyInfo describes one key of a runner.
type KeyInfo struct {
	Key string `json:"key"`
	// Depth is the number of queued messages, not counting the one being handled.
	Depth int `json:"depth"`
	// OldestAge is how long the oldest queued message has waited.
	OldestAge time.Duration `json:"oldest_age_ns"`
	Children  int           `json:"children"`
	// Timers is the number of timers pending for the key.
	Timers    int    `json:"timers"`
	Running   bool   `json:"running"`
	Handled   uint64 `json:"handled"`
	Panics    uint64 `json:"panics"`
	Coalesced uint64 `json:"coalesced"`
	Dropped   uint64 `json:"dropped"`
	Refused   uint64 `json:"refused"`
}

// Info describes a runner and its keys.
type Info struct {
	Name    string    `json:"name"`
	State   string    `json:"state"`
	Keys    []KeyInfo `json:"keys"`
	Handled uint64    `json:"handled"`
	Panics  uint64    `json:"panics"`
	// ChildPanics counts the child tasks that panicked.
	ChildPanics uint64 `json:"child_panics"`
	// Coalesced, Dropped and Refused count the messages the overflow policy replaced, pushed out and refused.
	Coalesced uint64 `json:"coalesced"`
	Dropped   uint64 `json:"dropped"`
	Refused   uint64 `json:"refused"`
}

// Snapshot describes the runner: its state and, for each key that has a
// goroutine, queued messages or work in flight, the mailbox depth, the age of
// the oldest queued message and whether the key's goroutine is running.
func (r *Runner[K, M, S]) Snapshot() Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	info := Info{
		Name:        r.name,
		State:       r.lifecycle.Load().String(),
		Keys:        make([]KeyInfo, 0, len(r.keys)),
		Handled:     r.totalHandled.Load(),
		Panics:      r.totalPanics.Load(),
		ChildPanics: r.totalChildPanics.Load(),
		Coalesced:   r.totalCoalesced.Load(),
		Dropped:     r.totalDropped.Load(),
		Refused:     r.totalRefused.Load(),
	}
	for _, a := range r.keys {
		key := KeyInfo{
			Key:       fmt.Sprint(a.key),
			Depth:     len(a.queue),
			Children:  a.children,
			Timers:    a.timers,
			Running:   a.running,
			Handled:   a.handled,
			Panics:    a.panics,
			Coalesced: a.coalesced,
			Dropped:   a.dropped,
			Refused:   a.refused,
		}
		if len(a.queue) > 0 {
			key.OldestAge = now.Sub(a.queue[0].at)
		}
		info.Keys = append(info.Keys, key)
	}
	slices.SortFunc(info.Keys, func(x, y KeyInfo) int { return strings.Compare(x.Key, y.Key) })
	return info
}
