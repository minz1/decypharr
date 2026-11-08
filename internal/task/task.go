// Package task gives every goroutine an owner.
//
// A Group is a named node in a tree. Tasks started with Go run on the
// group's context, so stopping a group cancels everything it owns, and a
// group that is no longer running refuses new tasks. That refusal is the
// single "closed" signal for a service: code never keeps its own closed flag
// next to a goroutine it started.
//
// A group's lifecycle is an explicit state machine:
//
//	Running → Stopping → Stopped   (Stop or Close)
//	Running → Failing  → Failed    (a task failed the group)
//
// Stop stops child groups first, newest first, then cancels the group's own
// tasks and waits for them. Close runs the same ordered sequence without
// waiting, for a task that must stop its own group or an ancestor. A failure
// (a Fatal error, or any error or panic from a GoCritical task) runs it too,
// and fails the parent, up to the root, so the owner of the root sees it
// (Failed fires at once; Done when the sequence ends).
// A context cancelled from outside (a signal) cancels the whole subtree at
// once; Stop still waits in the same order.
package task

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// ErrStopped is returned by Go on a group that is no longer running (wrapped
// with the failure when the group failed), and is the cancellation cause of
// a group's context after Stop or Close.
var ErrStopped = errors.New("task group stopped")

// ErrStopTimeout is returned by Stop when tasks were still running at the
// deadline. The error names them.
var ErrStopTimeout = errors.New("task group stop timed out")

// State is a group's lifecycle state.
type State uint8

const (
	// Running accepts new tasks and children.
	Running State = iota + 1
	// Stopping refuses new tasks and waits for the running ones.
	Stopping
	// Stopped has no tasks left after a stop.
	Stopped
	// Failing refuses new tasks: a task failed the group, which is closing.
	Failing
	// Failed has no tasks left after a failure. Err reports why.
	Failed
)

func (s State) String() string {
	switch s {
	case Running:
		return "running"
	case Stopping:
		return "stopping"
	case Stopped:
		return "stopped"
	case Failing:
		return "failing"
	case Failed:
		return "failed"
	default:
		return fmt.Sprintf("State(%d)", uint8(s))
	}
}

// successors lists the states s may move to: the transition table.
// transition refuses every other move.
func (s State) successors() []State {
	switch s {
	case Running:
		return []State{Stopping, Failing}
	case Stopping:
		return []State{Stopped}
	case Failing:
		return []State{Failed}
	case Stopped, Failed:
		return nil
	}
	return nil
}

func legal(from, to State) bool {
	return slices.Contains(from.successors(), to)
}

// settled is the state a closing state ends in once every task has returned.
func (s State) settled() State {
	if s == Failing {
		return Failed
	}
	return Stopped
}

// fatalError marks a task error that must fail its group.
type fatalError struct{ err error }

func (f fatalError) Error() string { return f.err.Error() }
func (f fatalError) Unwrap() error { return f.err }

// Fatal wraps err so that returning it from a task fails the task's group
// (and its ancestors) with err as the cause. Other task errors are logged
// and the group keeps running.
func Fatal(err error) error {
	if err == nil {
		return nil
	}
	if _, already := errors.AsType[fatalError](err); already {
		return err
	}
	return fatalError{err: err}
}

// Info describes a running task.
type Info struct {
	Path    string
	Started time.Time
}

type running struct {
	name    string
	started time.Time
}

// Group owns a set of tasks and child groups.
type Group struct {
	path   string
	parent *Group
	seq    uint64 // creation order among the parent's children
	logger zerolog.Logger
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	state    State
	failure  error
	children map[*Group]struct{}
	childSeq uint64
	tasks    map[uint64]running
	nextID   uint64
	wg       sync.WaitGroup
	done     chan struct{} // closed when the shutdown sequence ends
	failed   chan struct{} // closed when a task fails the group

	shutdownStarted bool // the ordered shutdown has been started (guarded by mu)

	hurry     chan struct{} // closed to make the shutdown sequence stop waiting
	hurryOnce sync.Once
}

// NewRoot returns a running group whose context derives from ctx.
func NewRoot(ctx context.Context, name string, logger zerolog.Logger) *Group {
	return newGroup(ctx, name, logger)
}

func newGroup(parent context.Context, path string, logger zerolog.Logger) *Group {
	ctx, cancel := context.WithCancelCause(parent)
	return &Group{
		path:     path,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		state:    Running,
		children: make(map[*Group]struct{}),
		tasks:    make(map[uint64]running),
		done:     make(chan struct{}),
		failed:   make(chan struct{}),
		hurry:    make(chan struct{}),
	}
}

// Child returns a new group owned by g. A child of a group that is no
// longer running (stopping, stopped, failing or failed) is created already
// stopped, with no failure of its own, so its Go refuses work.
func (g *Group) Child(name string) *Group {
	g.mu.Lock()
	defer g.mu.Unlock()
	child := newGroup(g.ctx, g.path+"/"+name, g.logger)
	child.parent = g
	if g.state != Running {
		child.cancel(ErrStopped)
		child.state = Stopped
		child.shutdownStarted = true
		close(child.done)
		return child
	}
	g.childSeq++
	child.seq = g.childSeq
	g.children[child] = struct{}{}
	return child
}

// Context is the group's context. It ends when the shutdown sequence
// reaches the group's own tasks (after its children have stopped), or when
// the parent's context ends. Use it for synchronous work done on the
// group's behalf instead of [context.Background].
func (g *Group) Context() context.Context { return g.ctx }

// Done is closed once every task in the subtree has returned after a stop
// or a failure.
func (g *Group) Done() <-chan struct{} { return g.done }

// Failed is closed as soon as a task fails the group, before the ordered
// shutdown that follows has finished. Err then reports the failure. That
// shutdown has no deadline of its own: the owner should follow with
// Stop(ctx) to bound it.
func (g *Group) Failed() <-chan struct{} { return g.failed }

// Cause reports why the group's context ended, or nil while it is live.
func (g *Group) Cause() error { return context.Cause(g.ctx) }

// Err reports the failure that failed the group, or nil if it has not
// failed (a clean stop is not an error). A stopped group with a non-nil Err
// had a critical task fail during its stop: a dirty shutdown.
func (g *Group) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failure
}

// State reports the group's lifecycle state.
func (g *Group) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// transition moves g to the given state if the table allows it. The caller
// holds g.mu.
func (g *Group) transition(to State) bool {
	if !legal(g.state, to) {
		return false
	}
	g.state = to
	return true
}

// Go runs fn in a new goroutine owned by g. When g is not running it starts
// nothing and returns ErrStopped, wrapping the failure if g failed.
//
// fn receives the group's context. A panic in fn is recovered and logged
// with its stack. An error is logged, unless it only reports that the group
// was stopped. A Fatal error fails g.
func (g *Group) Go(name string, fn func(ctx context.Context) error) error {
	return g.start(name, fn, false)
}

// GoCritical is Go for a task the group cannot run without, such as a
// service's serve loop: any error it returns, and any panic, fails the
// group as Fatal would. Returning nil is a clean exit. A serve loop should
// map its own "closed" error (http.ErrServerClosed) to nil. A failure fails
// every ancestor up to the root, so use it only for tasks whose failure
// must end the whole tree; per-key or per-request work uses Go.
func (g *Group) GoCritical(name string, fn func(ctx context.Context) error) error {
	return g.start(name, fn, true)
}

func (g *Group) start(name string, fn func(ctx context.Context) error, critical bool) error {
	id, err := g.admit(name)
	if err != nil {
		return err
	}
	go g.run(id, name, fn, critical)
	return nil
}

// Enter leases a slot in g for work that runs on a goroutine g does not
// own, such as a shared worker running a closure on g's behalf. Until leave
// is called the lease counts as one of g's tasks: Snapshot lists it and
// g's shutdown waits for it after cancelling g's context, so the work must
// watch g.Context(). When g is not running Enter returns ErrStopped, as Go
// does, and no leave. leave is safe to call more than once.
func (g *Group) Enter(name string) (func(), error) {
	id, err := g.admit(name)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { g.release(id) }) }, nil
}

// admit registers a task or lease named name, or refuses it when g is not
// running.
func (g *Group) admit(name string) (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != Running || g.ctx.Err() != nil {
		if g.failure != nil {
			return 0, fmt.Errorf("%w: %w", ErrStopped, g.failure)
		}
		return 0, ErrStopped
	}
	g.nextID++
	id := g.nextID
	g.tasks[id] = running{name: name, started: time.Now()}
	g.wg.Add(1)
	return id, nil
}

// release ends the task or lease admitted as id.
func (g *Group) release(id uint64) {
	g.mu.Lock()
	delete(g.tasks, id)
	g.mu.Unlock()
	g.wg.Done()
}

func (g *Group) run(id uint64, name string, fn func(ctx context.Context) error, critical bool) {
	defer g.release(id)
	err := g.call(name, fn)
	if critical {
		err = Fatal(err)
	}
	g.report(name, err)
}

// call runs fn, turning a panic into an error so one bad task cannot take
// the process down.
func (g *Group) call(name string, fn func(ctx context.Context) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			g.logger.Error().
				Str("task", g.path+":"+name).
				Interface("panic", recovered).
				Bytes("stack", debug.Stack()).
				Msg("Task panicked")
			err = panicError{value: recovered}
		}
	}()
	return fn(g.ctx)
}

// panicError carries a recovered panic. It is already logged with its
// stack, so report only acts on it when the task is critical.
type panicError struct{ value any }

func (e panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

func (g *Group) report(name string, err error) {
	if err == nil {
		return
	}
	cause := err
	fatal, isFatal := errors.AsType[fatalError](err)
	if isFatal {
		cause = fatal.err
	}
	if g.stopNoise(cause) {
		return
	}
	switch _, panicked := errors.AsType[panicError](cause); {
	case isFatal:
		if g.fail(cause) {
			g.logger.Error().Err(cause).Str("task", g.path+":"+name).Msg("Task failed its group")
		} else {
			g.logger.Error().Err(cause).Str("task", g.path+":"+name).Msg("Task failed during stop")
		}
	case panicked:
		// Already logged with its stack by call.
	default:
		g.logger.Error().Err(err).Str("task", g.path+":"+name).Msg("Task failed")
	}
}

// stopNoise reports whether err only says that g's context ended: every
// branch of its chain reaches the context's own error or cause. An error
// that carries anything else (a real failure joined with the cancellation)
// is not noise, and neither is a cancellation while the context is live.
func (g *Group) stopNoise(err error) bool {
	if g.ctx.Err() == nil {
		return false
	}
	return onlyEnds(err, g.ctx.Err(), context.Cause(g.ctx), ErrStopped)
}

// joinedError is an error that wraps several, such as the result of [errors.Join].
type joinedError interface {
	error
	Unwrap() []error
}

// onlyEnds reports whether err is one of ends, or wraps only ends: a joined
// error counts only if every one of its branches does.
func onlyEnds(err error, ends ...error) bool {
	if joined, ok := errors.AsType[joinedError](err); ok {
		branches := joined.Unwrap()
		for _, branch := range branches {
			if !onlyEnds(branch, ends...) {
				return false
			}
		}
		return len(branches) > 0
	}
	return slices.ContainsFunc(ends, func(end error) bool { return errors.Is(err, end) })
}

// fail records that a task failed g, moves g to Failing, fires Failed and
// fails g's parent. It reports whether it failed g: only the first failure
// of a running group does.
//
// The ordered shutdown is started by the highest ancestor that the failure
// moved to Failing (the root, or the first one that was no longer running),
// so a failing group is closed in its place in that sequence, after its
// newer siblings, instead of shutting itself down ahead of them.
//
// A failure while g is no longer running is recorded on g and every
// ancestor that has no failure yet (Err reports it, so the owner of the
// root can see a dirty stop), but fires no Failed and fails nothing.
func (g *Group) fail(cause error) bool {
	g.mu.Lock()
	if !g.transition(Failing) {
		g.mu.Unlock()
		g.recordDirty(cause)
		return false
	}
	g.failure = cause
	g.mu.Unlock()

	close(g.failed)
	takenOver := g.parent != nil && g.parent.fail(fmt.Errorf("%s: %w", g.path, cause))
	if !takenOver {
		g.startShutdown()
	}
	return true
}

// recordDirty records a failure that happened while g was no longer running
// on g and on each ancestor that has no failure yet.
func (g *Group) recordDirty(cause error) {
	for group := g; group != nil; group = group.parent {
		group.mu.Lock()
		if group.failure == nil {
			group.failure = cause
		}
		group.mu.Unlock()
		cause = fmt.Errorf("%s: %w", group.path, cause)
	}
}

// childrenLocked returns g's children oldest first. The caller holds g.mu.
func (g *Group) childrenLocked() []*Group {
	children := make([]*Group, 0, len(g.children))
	for child := range g.children {
		children = append(children, child)
	}
	slices.SortFunc(children, func(a, b *Group) int { return cmp.Compare(a.seq, b.seq) })
	return children
}

// beginStop moves a running g to Stopping, then starts g's ordered shutdown
// unless it has already started. A failing group whose ancestor took over
// its shutdown starts it here, when that ancestor's sequence closes it.
func (g *Group) beginStop() {
	g.mu.Lock()
	g.transition(Stopping)
	g.mu.Unlock()
	g.startShutdown()
}

// startShutdown runs g's ordered shutdown once: with ErrStopped as the
// cause for a stop, or the failure for a failing group.
func (g *Group) startShutdown() {
	g.mu.Lock()
	if g.shutdownStarted || g.state == Running {
		g.mu.Unlock()
		return
	}
	g.shutdownStarted = true
	cause := ErrStopped
	if g.state == Failing {
		cause = g.failure
	}
	children := g.childrenLocked()
	g.mu.Unlock()
	go g.shutdown(children, cause)
}

// shutdown is the one ordered sequence every stop and failure runs, once
// per group: close each child, newest first, and wait for it; then cancel
// g's own tasks with cause and wait for them; then settle g's state, detach
// g from its parent and close Done. hurry skips the waiting between
// children, so a deadline still reaches every task. No child can be added
// once g has left Running, so children is complete.
func (g *Group) shutdown(children []*Group, cause error) {
	for _, child := range slices.Backward(children) {
		child.Close()
		select {
		case <-child.done:
		case <-g.hurry:
		}
	}
	g.cancel(cause)
	g.wg.Wait()
	for _, child := range children {
		<-child.done
	}
	g.mu.Lock()
	g.transition(g.state.settled())
	g.mu.Unlock()
	if g.parent != nil {
		g.parent.removeChild(g)
	}
	close(g.done)
}

// rush makes g's shutdown, and its children's, stop waiting between steps,
// so every task in the subtree is cancelled at once.
func (g *Group) rush() {
	g.hurryOnce.Do(func() { close(g.hurry) })
	g.mu.Lock()
	children := g.childrenLocked()
	g.mu.Unlock()
	for _, child := range children {
		child.rush()
	}
}

// Close stops g without waiting, in the same order as Stop: children
// newest first, each waited for, then g's own tasks. It returns at once;
// Done closes when the sequence ends, which has no deadline: an owner that
// must bound it calls Stop(ctx) as well. Unlike Stop it is safe to call
// from a task of g or of a descendant.
func (g *Group) Close() { g.beginStop() }

// Stop stops g and waits: children newest first, each waited for, then g's
// own tasks. It returns nil once every task in the subtree has returned. If
// ctx ends first, the rest of the sequence runs at once (every remaining
// task is cancelled together) and Stop returns an ErrStopTimeout naming the
// tasks still running; they keep their cancelled context and Done closes
// once they return, so an owner must wait on Done (or exit) before reusing
// what they hold. Stop is safe to call more than once and from several
// goroutines, but never from a task of g or of a descendant (it would wait
// for itself): use Close there. On a failing group whose shutdown an
// ancestor has taken over, Stop (like Close) starts that shutdown now rather
// than at its turn in the ancestor's sequence; the root's owner, which is
// the usual caller, only waits.
func (g *Group) Stop(ctx context.Context) error {
	g.beginStop()
	select {
	case <-g.done:
		return nil
	case <-ctx.Done():
		select {
		case <-g.done:
			return nil
		default:
			g.rush()
			return g.timeoutError()
		}
	}
}

func (g *Group) removeChild(child *Group) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.children, child)
}

func (g *Group) timeoutError() error {
	infos := g.Snapshot()
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Path)
	}
	return fmt.Errorf("%w: %s still running: %s", ErrStopTimeout, g.path, strings.Join(names, ", "))
}

// Snapshot lists the running tasks of g and its children, sorted by path.
func (g *Group) Snapshot() []Info {
	g.mu.Lock()
	infos := make([]Info, 0, len(g.tasks))
	for _, task := range g.tasks {
		infos = append(infos, Info{Path: g.path + ":" + task.name, Started: task.started})
	}
	children := g.childrenLocked()
	g.mu.Unlock()

	for _, child := range children {
		infos = append(infos, child.Snapshot()...)
	}
	slices.SortFunc(infos, func(a, b Info) int { return strings.Compare(a.Path, b.Path) })
	return infos
}

// Detached returns a context that keeps ctx's values but not its
// cancellation, bounded by timeout. Use it for cleanup that must still run
// after the owner was cancelled, such as a final flush during Stop.
func Detached(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
