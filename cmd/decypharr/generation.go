package decypharr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
)

// generationDrainDeadline bounds the whole teardown of a generation. The
// service unit stops the process after TimeoutStopSec=30, so the services get
// 25 seconds and the rest is left for the process to exit.
const generationDrainDeadline = 25 * time.Second

// genState is the lifecycle of one generation of services.
//
//	Starting → Serving → Draining → Restarting | Stopped | Failed
//
// Starting also moves to Draining (a signal, a restart request or a service
// failure before the manager is ready) and to Failed (the services could not
// be built).
type genState uint8

const (
	// genStarting: the services are being built and the manager is starting.
	genStarting genState = iota + 1
	// genServing: the manager is ready and the listeners are up.
	genServing
	// genDraining: the services are being stopped, newest first.
	genDraining
	// genRestarting: drained for a restart; the next generation follows.
	genRestarting
	// genStopped: drained after a stop request.
	genStopped
	// genFailed: ended by a failure or a stop that timed out.
	genFailed
)

func (s genState) String() string {
	switch s {
	case genStarting:
		return "starting"
	case genServing:
		return "serving"
	case genDraining:
		return "draining"
	case genRestarting:
		return "restarting"
	case genStopped:
		return "stopped"
	case genFailed:
		return "failed"
	default:
		return fmt.Sprintf("genState(%d)", uint8(s))
	}
}

// successors is the transition table of a generation.
func (s genState) successors() []genState {
	switch s {
	case genStarting:
		return []genState{genServing, genDraining, genFailed}
	case genServing:
		return []genState{genDraining}
	case genDraining:
		return []genState{genRestarting, genStopped, genFailed}
	case genRestarting, genStopped, genFailed:
		return nil
	}
	return nil
}

// generation is one run of the services under its own task root. It owns the
// restart token of that run: a request is idempotent and never outlives the
// generation, so nothing is shared with the next one.
type generation struct {
	root *task.Group

	log zerolog.Logger
	// deadline bounds the whole teardown; the supervisor sets it before the
	// services are built.
	deadline time.Duration

	mu         sync.Mutex
	state      genState
	restart    chan struct{} // closed by the first request of the current token
	requested  bool
	// again records a request made while the current one was pending: its
	// configuration may be newer than the one being loaded.
	again      bool
	ready      <-chan struct{}
	onRejected func(error)

	stopOnce   sync.Once
	stopCtx    context.Context
	stopCancel context.CancelFunc
}

func newGeneration(root *task.Group, log zerolog.Logger) *generation {
	return &generation{
		root:     root,
		log:      log,
		deadline: generationDrainDeadline,
		state:    genStarting,
		restart:  make(chan struct{}),
	}
}

// stopContext is the context that bounds the teardown: it ends when the drain
// deadline passes, whoever starts the teardown first (the supervisor, or a
// service that notices the group failing). Services bound their own stop
// waits by it, so the whole stop fits the deadline.
func (g *generation) stopContext() context.Context {
	g.stopOnce.Do(func() {
		g.stopCtx, g.stopCancel = task.Detached(g.root.Context(), g.deadline)
	})
	return g.stopCtx
}

// releaseStop frees the stop context's timer once the teardown is over.
func (g *generation) releaseStop() {
	g.stopContext()
	g.stopCancel()
}

// State reports the generation's lifecycle state.
func (g *generation) State() genState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// transition is the one place the state changes: it refuses, and logs, every
// move the table does not list.
func (g *generation) transition(to genState) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !state.Legal(genState.successors, g.state, to) {
		g.log.Error().Stringer("from", g.state).Stringer("to", to).Msg("Refused an illegal generation transition")
		return false
	}
	g.state = to
	return true
}

// finish records how the generation ended. It does nothing when the generation
// is already in that state, as after a failed build.
func (g *generation) finish(to genState) {
	if g.State() == to {
		return
	}
	g.transition(to)
}

// WaitReady sets the channel whose closing means the services are ready to
// serve. Without it the generation is serving as soon as it is built. Call it
// from the build function.
func (g *generation) WaitReady(ready <-chan struct{}) { g.ready = ready }

// OnRestartRejected sets what to tell the owner of the configuration when a
// requested restart is refused because the next configuration does not load.
// Call it from the build function.
func (g *generation) OnRestartRejected(report func(error)) { g.onRejected = report }

// RequestRestart asks for the next generation. It never blocks, and a
// generation that is already draining ignores it: the next generation reads the
// configuration after the drain, so whatever was saved is picked up anyway. A
// second call while a request is pending is kept: if the pending request is
// rejected, the kept one loads the configuration again.
func (g *generation) RequestRestart() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != genStarting && g.state != genServing {
		return
	}
	if g.requested {
		g.again = true
		return
	}
	g.requested = true
	close(g.restart)
}

// restartRequested is closed once a restart was requested.
func (g *generation) restartRequested() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.restart
}

// rejectRestart records that a requested restart cannot happen and makes the
// next request effective again, so the generation keeps serving and a later
// request can retry. The token is re-armed before the owner is told, so a
// request made in reaction to that report is not swallowed by the old token.
// A request kept while the rejected one was pending is retried at once.
func (g *generation) rejectRestart(cause error) {
	g.mu.Lock()
	if g.state == genStarting || g.state == genServing {
		g.restart = make(chan struct{})
		g.requested = g.again
		g.again = false
		if g.requested {
			close(g.restart)
		}
	}
	report := g.onRejected
	g.mu.Unlock()
	if report != nil {
		report(cause)
	}
}

// buildFunc starts one generation's services as children of gen.root, in
// dependency order, and returns once they are started. The cleanup it returns
// runs after the tree has drained, even when it also returns an error.
type buildFunc func(gen *generation, cfg *config.Config, logs *logger.Factory) (cleanup func(), err error)

// supervisor runs generations one after another.
type supervisor struct {
	load          func() (*config.Config, error)
	logs          func(*config.Config) *logger.Factory
	build         buildFunc
	drainDeadline time.Duration
}

// endKind is why a generation stopped serving.
type endKind int

const (
	endStop    endKind = iota // the process was asked to stop
	endRestart                // a restart was requested and the next config loads
	endFailed                 // a service failed
)

// outcome is how a generation ended.
type outcome struct {
	state genState // genRestarting, genStopped or genFailed
	err   error
	next  *config.Config // the configuration of the next generation
}

// run loads the configuration and runs generations until the process is
// stopped (nil) or a generation fails (its error: the process exits non-zero
// and the service manager restarts it).
func (s supervisor) run(ctx context.Context) error {
	cfg, err := s.load()
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}
	for {
		out := s.runGeneration(ctx, cfg)
		switch out.state {
		case genRestarting:
			cfg = out.next
		case genStopped:
			return nil
		case genStarting, genServing, genDraining, genFailed:
			return out.err
		}
	}
}

// runGeneration builds the services from cfg, waits for the generation to
// end, drains it and says what follows.
//
// The task root does not derive from ctx: a signal must not cancel the whole
// tree at once, because the manager would be stopped while the server still
// serves requests that use it. The generation is stopped explicitly instead,
// newest service first, within the drain deadline.
func (s supervisor) runGeneration(ctx context.Context, cfg *config.Config) outcome {
	logs := s.logs(cfg)
	log := logs.New("decypharr")
	gen := newGeneration(task.NewRoot(context.WithoutCancel(ctx), "generation", log), log)
	gen.deadline = s.drainDeadline
	defer gen.releaseStop()

	cleanup, buildErr := s.build(gen, cfg, logs)
	kind, next := endFailed, (*config.Config)(nil)
	if buildErr != nil {
		log.Error().Err(buildErr).Msg("Services could not be started")
		gen.transition(genFailed)
	} else {
		kind, next = s.await(ctx, gen, log)
		gen.transition(genDraining)
	}

	stopErr := gen.root.Stop(gen.stopContext())
	if cleanup != nil {
		cleanup()
	}

	// A failure that fired before the stop began wins over a stop or a restart
	// that was picked at the same moment: the process must exit non-zero.
	if kind != endFailed {
		select {
		case <-gen.root.Failed():
			kind = endFailed
		default:
		}
	}
	failure := buildErr
	if buildErr != nil && gen.root.Err() != nil {
		// A service that failed to start stops the root, and the services built after it only find it stopped.
		// The failure that stopped the root is the cause; the refusal is its echo.
		failure = errors.Join(gen.root.Err(), buildErr)
	}
	if kind == endFailed && failure == nil {
		failure = gen.root.Err()
		log.Error().Err(failure).Msg("Service stopped unexpectedly")
	}
	if stopErr != nil {
		log.Error().Err(stopErr).Msg("Services did not stop in time; not starting another generation")
		gen.finish(genFailed)
		return outcome{state: genFailed, err: errors.Join(failure, stopErr)}
	}
	if failure != nil {
		gen.finish(genFailed)
		return outcome{state: genFailed, err: failure}
	}
	if err := gen.root.Err(); err != nil {
		log.Warn().Err(err).Msg("A service failed while the generation was stopping")
	}
	// A signal that arrived while a restart was draining stops the process: no
	// new generation is built on a cancelled context.
	if kind == endStop || ctx.Err() != nil {
		gen.finish(genStopped)
		log.Info().Msg("Decypharr has been stopped gracefully.")
		return outcome{state: genStopped}
	}

	// The old services are gone, so anything saved while they drained is on
	// disk now. If it no longer loads, fall back to what was validated before
	// the drain.
	gen.finish(genRestarting)
	after, err := s.load()
	if err != nil {
		log.Error().Err(err).
			Msg("Configuration saved during the restart does not load; using the one validated before it")
		after = next
	}
	log.Info().Msg("Decypharr is restarting")
	return outcome{state: genRestarting, next: after}
}

// await waits until the generation has to end. A restart request first loads
// the next configuration: if that fails the generation keeps serving, and the
// owner of the configuration is told, instead of draining into a process that
// cannot start again.
func (s supervisor) await(ctx context.Context, gen *generation, log zerolog.Logger) (endKind, *config.Config) {
	ready := gen.ready
	if ready == nil {
		gen.transition(genServing)
	}
	for {
		select {
		case <-ready:
			gen.transition(genServing)
			ready = nil
		case <-ctx.Done():
			return endStop, nil
		case <-gen.restartRequested():
			next, err := s.load()
			if err != nil {
				log.Error().Err(err).
					Msg("Restart cancelled: the configuration does not load; the running services keep serving")
				gen.rejectRestart(err)
				continue
			}
			return endRestart, next
		case <-gen.root.Failed():
			return endFailed, nil
		}
	}
}
