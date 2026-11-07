// Package keyed runs one actor per key.
//
// An actor is a goroutine that owns the state for its key. Messages for a key
// are handled one at a time, in the order they were accepted, by that key's
// goroutine, which is a task of the runner's group. The goroutine starts with
// the first message, exits after a quiet period, and starts again with the
// next one; a handler therefore keeps no state in its goroutine, and the state
// of a key lives wherever the handler keeps it (a store, a map the handler
// owns).
//
// A handler must not block on I/O. Slow work runs as a child task
// ([Effects.Go]) bound to the runner's context, and reports back with a
// message. Actors talk to each other with fire-and-forget [Effects.Send]. A
// caller outside the handlers can [Runner.Ask] for a reply with a timeout; an
// Ask from inside a handler is refused, because the handler would wait for
// itself.
//
// Mailboxes are bounded. What happens when one is full depends on the class
// of the message ([Class]): a command is refused so the caller can try again
// later, an observation is coalesced with, or replaces, an older one, and a
// timer is never dropped. Timers belong to the runner, not to the goroutine,
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

// Handler handles the messages of a runner.
type Handler[K comparable, M any] interface {
	// Handle processes one message for key. It runs on the key's goroutine,
	// one message at a time. It must not block on I/O: slow work runs as a
	// child task (fx.Go) and reports back with a message. ctx ends when the
	// runner stops.
	Handle(ctx context.Context, key K, msg M, fx *Effects[K, M])
}

// HandlerFunc makes a Handler of a function.
type HandlerFunc[K comparable, M any] func(ctx context.Context, key K, msg M, fx *Effects[K, M])

// Handle calls f.
func (f HandlerFunc[K, M]) Handle(ctx context.Context, key K, msg M, fx *Effects[K, M]) {
	f(ctx, key, msg, fx)
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
	msg M
	at  time.Time
	// bypass marks a message that does not count against the mailbox bound: a timer, or a child task's result.
	bypass bool
}

// actor is the runner's record of one key. The runner's mutex guards every field.
type actor[K comparable, M any] struct {
	key      K
	queue    []envelope[M]
	bypassed int // envelopes in queue that bypass the bound
	running  bool
	wake     chan struct{}
	children int // child tasks in flight
	handled  uint64
	panics   uint64
}

func (a *actor[K, M]) depth() int { return len(a.queue) - a.bypassed }

// Runner runs one actor per key.
type Runner[K comparable, M any] struct {
	name      string
	group     *task.Group
	handler   Handler[K, M]
	opts      Options[M]
	logger    zerolog.Logger
	noIdle    bool
	lifecycle *state.Atomic[runState]
	ctx       context.Context

	mu   sync.Mutex
	keys map[K]*actor[K, M]
	live int // key goroutines running or starting

	totalHandled atomic.Uint64
	totalPanics  atomic.Uint64
}

// New makes a runner whose key goroutines are tasks of a child of g named
// name. Stopping g stops the runner. A runner made on a group that is not
// running refuses every message.
func New[K comparable, M any](g *task.Group, name string, h Handler[K, M], opts Options[M]) *Runner[K, M] {
	if opts.MailboxSize <= 0 {
		opts.MailboxSize = defaultMailboxSize
	}
	if opts.IdleAfter == 0 {
		opts.IdleAfter = defaultIdleAfter
	}
	child := g.Child(name)
	r := &Runner[K, M]{
		name:      name,
		group:     child,
		handler:   h,
		opts:      opts,
		logger:    opts.Logger,
		noIdle:    opts.IdleAfter < 0,
		lifecycle: state.NewAtomic(runRunning, runSuccessors),
		ctx:       child.Context(),
		keys:      make(map[K]*actor[K, M]),
	}
	if err := child.Go("supervisor", r.supervise); err != nil {
		r.lifecycle.Transition(runRunning, runStopping)
		r.lifecycle.Transition(runStopping, runStopped)
	}
	return r
}

// NewSingleton makes a runner with the one key and no idle exit: its goroutine
// runs until the runner stops.
func NewSingleton[M any](g *task.Group, name string, h Handler[struct{}, M], opts Options[M]) *Runner[struct{}, M] {
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
	return r
}

// supervise is the runner's own task: it ends the runner when the group stops.
func (r *Runner[K, M]) supervise(ctx context.Context) error {
	<-ctx.Done()
	r.beginStop()
	return nil
}

// beginStop moves the runner to Stopping, and on to Stopped once no key
// goroutine is left.
func (r *Runner[K, M]) beginStop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lifecycle.Transition(runRunning, runStopping)
	r.settleLocked()
}

// settleLocked moves a stopping runner with no goroutine left to Stopped.
func (r *Runner[K, M]) settleLocked() {
	if r.live == 0 {
		r.lifecycle.Transition(runStopping, runStopped)
	}
}

// accepting reports whether the runner takes messages. The group's context
// ends before the supervisor task has marked the runner as stopping, so both
// are checked.
func (r *Runner[K, M]) accepting() bool {
	return r.lifecycle.Load() == runRunning && r.ctx.Err() == nil
}

// Send hands msg to key's actor without waiting for it. It returns ErrStopped
// once the runner stops and ErrMailboxFull when the key's mailbox has no room
// for a message of its class.
func (r *Runner[K, M]) Send(key K, msg M) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enqueueLocked(key, envelope[M]{msg: msg, at: time.Now()})
}

// enqueueLocked queues env for key and starts the key's goroutine if it is not running.
func (r *Runner[K, M]) enqueueLocked(key K, env envelope[M]) error {
	if !r.accepting() {
		return ErrStopped
	}
	a := r.actorLocked(key)
	if !env.bypass && a.depth() >= r.opts.MailboxSize {
		r.dropIfIdleLocked(a)
		return ErrMailboxFull
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
	if err := r.startLocked(a); err != nil {
		a.queue = a.queue[:len(a.queue)-1]
		if env.bypass {
			a.bypassed--
		}
		r.dropIfIdleLocked(a)
		return err
	}
	return nil
}

func (r *Runner[K, M]) actorLocked(key K) *actor[K, M] {
	a, ok := r.keys[key]
	if !ok {
		a = &actor[K, M]{key: key, wake: make(chan struct{}, 1)}
		r.keys[key] = a
	}
	return a
}

// dropIfIdleLocked forgets a key that has nothing queued, nothing running and nothing in flight.
func (r *Runner[K, M]) dropIfIdleLocked(a *actor[K, M]) {
	if !a.running && len(a.queue) == 0 && a.children == 0 && !r.noIdle {
		delete(r.keys, a.key)
	}
}

// startLocked starts the key's goroutine.
func (r *Runner[K, M]) startLocked(a *actor[K, M]) error {
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
func (r *Runner[K, M]) loop(ctx context.Context, a *actor[K, M]) {
	hctx := withHandlerMarker(ctx)
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
			r.mu.Unlock()
			r.handle(hctx, a, env)
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
			if len(a.queue) == 0 {
				r.exitLocked(a, false)
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
		}
	}
}

// exitLocked ends the key's goroutine. On a stop the queue is dropped.
func (r *Runner[K, M]) exitLocked(a *actor[K, M], stopping bool) {
	if stopping {
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
func (r *Runner[K, M]) handle(ctx context.Context, a *actor[K, M], env envelope[M]) {
	fx := &Effects[K, M]{r: r, key: a.key}
	defer func() {
		recovered := recover()
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
	r.handler.Handle(ctx, a.key, env.msg, fx)
}

// Effects is what a handler can do besides change its own state. It is valid
// only inside the Handle call it was passed to.
type Effects[K comparable, M any] struct {
	r   *Runner[K, M]
	key K
}

// Go runs fn as a child task of the key's actor, on the runner's context, and
// sends the message it returns back to the same key. The message is queued
// past the mailbox bound: the actor asked for it, so it is never refused. If
// fn panics no message is sent and the panic is logged. Go returns ErrStopped
// once the runner stops.
func (fx *Effects[K, M]) Go(name string, fn func(ctx context.Context) M) error {
	r, key := fx.r, fx.key
	r.mu.Lock()
	if !r.accepting() {
		r.mu.Unlock()
		return ErrStopped
	}
	a := r.actorLocked(key)
	a.children++
	r.mu.Unlock()

	err := r.group.Go(fmt.Sprint(key)+"/"+name, func(ctx context.Context) error {
		defer r.childDone(key)
		msg := fn(withHandlerMarker(ctx))
		r.mu.Lock()
		defer r.mu.Unlock()
		if deliverErr := r.enqueueLocked(key, envelope[M]{msg: msg, at: time.Now(), bypass: true}); deliverErr != nil {
			r.logger.Debug().Err(deliverErr).Str("runner", r.name).Str("key", fmt.Sprint(key)).
				Str("task", name).Msg("A child task's result was not delivered")
		}
		return nil
	})
	if err != nil {
		r.childDone(key)
		return ErrStopped
	}
	return nil
}

func (r *Runner[K, M]) childDone(key K) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.keys[key]; ok {
		a.children--
		r.dropIfIdleLocked(a)
	}
}

// Send hands msg to another key of the same runner, without waiting for it.
func (fx *Effects[K, M]) Send(key K, msg M) error {
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
	Running   bool          `json:"running"`
	Handled   uint64        `json:"handled"`
	Panics    uint64        `json:"panics"`
}

// Info describes a runner and its keys.
type Info struct {
	Name    string    `json:"name"`
	State   string    `json:"state"`
	Keys    []KeyInfo `json:"keys"`
	Handled uint64    `json:"handled"`
	Panics  uint64    `json:"panics"`
}

// Snapshot describes the runner: its state and, for each key that has a
// goroutine, queued messages or work in flight, the mailbox depth, the age of
// the oldest queued message and whether the key's goroutine is running.
func (r *Runner[K, M]) Snapshot() Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	info := Info{
		Name:    r.name,
		State:   r.lifecycle.Load().String(),
		Keys:    make([]KeyInfo, 0, len(r.keys)),
		Handled: r.totalHandled.Load(),
		Panics:  r.totalPanics.Load(),
	}
	for _, a := range r.keys {
		key := KeyInfo{
			Key:      fmt.Sprint(a.key),
			Depth:    len(a.queue),
			Children: a.children,
			Running:  a.running,
			Handled:  a.handled,
			Panics:   a.panics,
		}
		if len(a.queue) > 0 {
			key.OldestAge = now.Sub(a.queue[0].at)
		}
		info.Keys = append(info.Keys, key)
	}
	slices.SortFunc(info.Keys, func(x, y KeyInfo) int { return strings.Compare(x.Key, y.Key) })
	return info
}
