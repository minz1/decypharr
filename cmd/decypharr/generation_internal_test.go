package decypharr

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/state/statetest"
	"github.com/sirrobot01/decypharr/internal/task"
)

const waitFor = 10 * time.Second

func TestGenerationTransitionTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, genState.successors,
		[]genState{genStarting, genServing, genDraining, genRestarting, genStopped, genFailed},
		[]statetest.Edge[genState]{
			{From: genStarting, To: genServing},
			{From: genStarting, To: genDraining},
			{From: genStarting, To: genFailed},
			{From: genServing, To: genDraining},
			{From: genDraining, To: genRestarting},
			{From: genDraining, To: genStopped},
			{From: genDraining, To: genFailed},
		})
}

func TestGenerationRefusesIllegalTransitions(t *testing.T) {
	t.Parallel()
	gen := newGeneration(task.NewRoot(t.Context(), "generation", zerolog.Nop()), zerolog.Nop())
	if gen.transition(genRestarting) {
		t.Fatal("starting -> restarting was allowed: a generation must drain first")
	}
	if !gen.transition(genServing) || !gen.transition(genDraining) {
		t.Fatal("serving then draining refused")
	}
	if gen.transition(genServing) {
		t.Fatal("draining -> serving was allowed")
	}
	if got := gen.State(); got != genDraining {
		t.Fatalf("state = %v, want draining", got)
	}
}

// Restart is a token owned by one generation: asking twice changes nothing,
// and the next generation starts with a fresh token.
func TestRestartIsIdempotentWithinAGeneration(t *testing.T) {
	t.Parallel()
	first := newGeneration(task.NewRoot(t.Context(), "generation", zerolog.Nop()), zerolog.Nop())
	var calls sync.WaitGroup
	for range 8 {
		calls.Go(first.RequestRestart)
	}
	calls.Wait()
	first.RequestRestart()
	select {
	case <-first.restartRequested():
	default:
		t.Fatal("restart was not recorded")
	}

	second := newGeneration(task.NewRoot(t.Context(), "generation", zerolog.Nop()), zerolog.Nop())
	select {
	case <-second.restartRequested():
		t.Fatal("a new generation inherited the previous generation's restart request")
	default:
	}
}

// harness drives a supervisor over fake services.
type harness struct {
	t     *testing.T
	mu    sync.Mutex
	built []string // Port of each generation's config, in build order
	loads atomic.Int32
	disk  atomic.Pointer[config.Config]
	gens  chan *generation
	order []string // events, in the order they happened
}

func configFor(t *testing.T, port string) *config.Config {
	t.Helper()
	cfg := config.New(t.TempDir())
	cfg.Port = port
	return cfg
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, gens: make(chan *generation, 8)}
	h.disk.Store(configFor(t, "v1"))
	return h
}

func (h *harness) event(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.order = append(h.order, name)
}

func (h *harness) events() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.order)
}

func (h *harness) supervisor(build buildFunc) supervisor {
	return supervisor{
		load: func() (*config.Config, error) {
			h.loads.Add(1)
			h.event("load")
			return h.disk.Load(), nil
		},
		logs:          func(*config.Config) *logger.Factory { return logger.Discard() },
		build:         build,
		drainDeadline: waitFor,
	}
}

func (h *harness) nextGeneration() *generation {
	h.t.Helper()
	select {
	case gen := <-h.gens:
		return gen
	case <-time.After(waitFor):
		h.t.Fatal("no generation was built")
		return nil
	}
}

func (h *harness) runAsync(ctx context.Context, sup supervisor) <-chan error {
	done := make(chan error, 1)
	go func() { done <- sup.run(ctx) }()
	return done
}

func awaitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(waitFor):
		t.Fatal("the supervisor did not return")
		return nil
	}
}

// service adds a child group with one task that runs until its context ends
// and then does onStop.
func service(t *testing.T, gen *generation, name string, onStop func()) {
	t.Helper()
	child := gen.root.Child(name)
	if err := child.Go("run", func(ctx context.Context) error {
		<-ctx.Done()
		onStop()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) record(cfg *config.Config, gen *generation) {
	h.mu.Lock()
	h.built = append(h.built, cfg.Port)
	h.mu.Unlock()
	h.gens <- gen
}

// A config saved while the old generation drains is in the next generation's
// config: the file is read after the drain, not when the restart is asked for.
func TestConfigSavedDuringDrainReachesTheNextGeneration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	build := func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		service(t, gen, "http", func() {
			// a save that lands after the restart was requested
			h.disk.Store(configFor(t, "v2"))
			h.event("drained")
		})
		return noop, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.runAsync(ctx, h.supervisor(build))

	first := h.nextGeneration()
	first.RequestRestart()
	h.nextGeneration()

	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	built := slices.Clone(h.built)
	h.mu.Unlock()
	if !slices.Equal(built, []string{"v1", "v2"}) {
		t.Fatalf("generations were built with %v, want [v1 v2]: the save during the drain was lost", built)
	}
	// load, load (validation before the drain), drained, load (after it)
	events := h.events()
	drained := slices.Index(events, "drained")
	lastLoad := -1
	for i, event := range events {
		if event == "load" {
			lastLoad = i
		}
	}
	if drained < 0 || lastLoad < drained {
		t.Fatalf("events = %v: the config was not re-read after the old generation drained", events)
	}
}

// Services stop newest first, whether the generation ends on a signal or a
// restart: a signal must not cancel the manager while the server still serves
// requests that use it. Cleanup runs after all of them.
func TestServicesStopNewestFirstThenCleanUp(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"signal", "restart"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			want := []string{"stopped share", "stopped mount", "stopped http", "stopped manager", "cleanup"}
			if got := shutdownOrder(t, ending == "restart"); !slices.Equal(got, want) {
				t.Fatalf("shutdown order = %v, want %v", got, want)
			}
		})
	}
}

// shutdownOrder runs a generation of four services, ends it with a signal
// (after a restart first when restart is set) and returns the stop events of
// the first generation in order.
func shutdownOrder(t *testing.T, restart bool) []string {
	t.Helper()
	h := newHarness(t)
	var builds atomic.Int32
	build := func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		if builds.Add(1) > 1 {
			return noop, nil
		}
		for _, name := range []string{"manager", "http", "mount", "share"} {
			service(t, gen, name, func() {
				h.event("stopped " + name)
				time.Sleep(10 * time.Millisecond)
			})
		}
		return func() { h.event("cleanup") }, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.runAsync(ctx, h.supervisor(build))
	first := h.nextGeneration()
	if restart {
		first.RequestRestart()
		h.nextGeneration()
	}
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
	var stops []string
	for _, event := range h.events() {
		if strings.HasPrefix(event, "stopped ") || event == "cleanup" {
			stops = append(stops, event)
		}
	}
	return stops
}

func noop() {}

// The generation serves only once the manager is ready; a restart asked for
// before that still drains it, and a failure before that fails it.
func TestServingWaitsForTheManagerToBeReady(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ready := make(chan struct{})
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		gen.WaitReady(ready)
		return noop, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.runAsync(ctx, sup)
	gen := h.nextGeneration()
	time.Sleep(50 * time.Millisecond)
	if got := gen.State(); got != genStarting {
		t.Fatalf("state = %v before the manager is ready, want starting", got)
	}
	close(ready)
	deadline := time.After(waitFor)
	for gen.State() != genServing {
		select {
		case <-deadline:
			t.Fatalf("state = %v after the manager became ready, want serving", gen.State())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
}

// A manager that fails to start fails the generation while it is still
// starting; the process exits with that error.
func TestManagerStartFailureWhileStartingFailsTheGeneration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	failure := errors.New("failed to start Arr indexer")
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		gen.WaitReady(make(chan struct{})) // never ready
		return nil, gen.root.Child("manager").GoCritical("run", func(context.Context) error { return failure })
	})
	err := awaitRun(t, h.runAsync(t.Context(), sup))
	if !errors.Is(err, failure) {
		t.Fatalf("run returned %v, want the manager failure", err)
	}
}

// A restart whose configuration does not load leaves the running services
// serving and tells the owner of the configuration; a later request retries.
func TestRestartWithUnloadableConfigKeepsServing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bad := errors.New(`shared_dir_mode "775x" is not an octal file mode`)
	var rejected atomic.Pointer[error]
	stopped := make(chan struct{}, 4)
	var loads atomic.Int32
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		gen.OnRestartRejected(func(err error) { rejected.Store(&err) })
		service(t, gen, "http", func() { stopped <- struct{}{} })
		return noop, nil
	})
	sup.load = func() (*config.Config, error) {
		if loads.Add(1) == 2 {
			return nil, bad
		}
		return h.disk.Load(), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.runAsync(ctx, sup)
	first := h.nextGeneration()
	first.RequestRestart()
	deadline := time.After(waitFor)
	for rejected.Load() == nil {
		select {
		case <-deadline:
			t.Fatal("the rejected restart was not reported")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !errors.Is(*rejected.Load(), bad) {
		t.Fatalf("reported %v, want the load failure", *rejected.Load())
	}
	select {
	case <-stopped:
		t.Fatal("the services were stopped for a restart that could not happen")
	default:
	}
	if got := first.State(); got != genServing {
		t.Fatalf("state = %v, want serving", got)
	}
	first.RequestRestart() // the retry
	h.nextGeneration()
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
}

// A restart requested while the previous request's configuration is still
// loading is kept: when that load is rejected, the kept request loads again and
// the next generation starts with what was saved in the meantime.
func TestRestartRequestedDuringARejectedLoadIsRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	loading, release := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		return noop, nil
	})
	sup.load = func() (*config.Config, error) {
		if loads.Add(1) == 2 {
			close(loading)
			<-release
			return nil, errors.New("config.json does not load")
		}
		return h.disk.Load(), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.runAsync(ctx, sup)
	first := h.nextGeneration()
	first.RequestRestart() // saves A, which does not load
	<-loading
	first.RequestRestart() // saves B while A loads
	close(release)
	h.nextGeneration()
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
}

// A critical service that fails ends the generation with its error, stops the
// other services and starts no next generation.
func TestCriticalServiceFailureEndsTheGenerationWithAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	failure := errors.New("listen tcp :8282: address already in use")
	siblingStopped := make(chan struct{})
	var cleaned atomic.Bool
	build := func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		service(t, gen, "manager", func() { close(siblingStopped) })
		serve := gen.root.Child("http")
		if err := serve.GoCritical("serve", func(context.Context) error { return failure }); err != nil {
			return nil, err
		}
		return func() { cleaned.Store(true) }, nil
	}
	err := awaitRun(t, h.runAsync(t.Context(), h.supervisor(build)))
	if !errors.Is(err, failure) {
		t.Fatalf("run returned %v, want the service failure", err)
	}
	select {
	case <-siblingStopped:
	default:
		t.Fatal("the failed generation returned before its other services stopped")
	}
	if !cleaned.Load() {
		t.Fatal("cleanup did not run after the failed generation")
	}
	if got := h.loads.Load(); got != 1 {
		t.Fatalf("config loaded %d times, want 1: a failed generation must not be followed by another", got)
	}
}

// Building the services can fail (a bad cache directory): the generation
// fails, what was started is stopped and cleanup runs.
func TestBuildFailureFailsTheGeneration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	failure := errors.New("share cache: permission denied")
	started := make(chan struct{})
	var cleaned atomic.Bool
	build := func(gen *generation, _ *config.Config, _ *logger.Factory) (func(), error) {
		service(t, gen, "manager", func() { close(started) })
		return func() { cleaned.Store(true) }, failure
	}
	err := awaitRun(t, h.runAsync(t.Context(), h.supervisor(build)))
	if !errors.Is(err, failure) {
		t.Fatalf("run returned %v, want the build failure", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("services started before the failure were left running")
	}
	if !cleaned.Load() {
		t.Fatal("cleanup did not run after a failed build")
	}
}

// A drain that exceeds its deadline is an error, and no next generation starts
// over services that are still running.
func TestStopTimeoutDoesNotStartTheNextGeneration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	build := func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		child := gen.root.Child("mount")
		err := child.Go("unmount", func(context.Context) error {
			<-release // ignores cancellation
			return nil
		})
		return nil, err
	}
	sup := h.supervisor(build)
	sup.drainDeadline = 50 * time.Millisecond
	done := h.runAsync(t.Context(), sup)
	h.nextGeneration().RequestRestart()
	err := awaitRun(t, done)
	if !errors.Is(err, task.ErrStopTimeout) {
		t.Fatalf("run returned %v, want a stop timeout", err)
	}
	if !strings.Contains(err.Error(), "mount:unmount") {
		t.Fatalf("error %q does not name the straggler", err)
	}
	if got := h.loads.Load(); got != 2 {
		t.Fatalf("config loaded %d times, want 2 (start, validation): no re-read after a stop timeout", got)
	}
	if len(h.gens) != 0 {
		t.Fatal("a second generation was built after a stop timeout")
	}
}

// The configuration is validated before the drain and read again after it. If
// the second read fails the old generation is already gone, so the next one
// starts from the configuration that was validated before the drain.
func TestReloadFailureAfterDrainFallsBackToTheValidatedConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var loads atomic.Int32
	drained := make(chan struct{})
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		service(t, gen, "http", func() {
			select {
			case <-drained:
			default:
				close(drained)
			}
		})
		return noop, nil
	})
	sup.load = func() (*config.Config, error) {
		switch loads.Add(1) {
		case 1:
			return configFor(t, "v1"), nil
		case 2: // validation before the drain
			return configFor(t, "validated"), nil
		default: // re-read after the drain
			select {
			case <-drained:
			default:
				t.Error("the configuration was re-read before the old generation drained")
			}
			return nil, errors.New("config.json is half written")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.runAsync(ctx, sup)
	h.nextGeneration().RequestRestart()
	h.nextGeneration()
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !slices.Equal(h.built, []string{"v1", "validated"}) {
		t.Fatalf("generations were built with %v, want [v1 validated]", h.built)
	}
}

// A signal stops the process cleanly: nil, and cleanup ran.
func TestSignalStopsTheProcessCleanly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var cleaned atomic.Bool
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		service(t, gen, "http", func() {})
		return func() { cleaned.Store(true) }, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.runAsync(ctx, sup)
	gen := h.nextGeneration()
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if !cleaned.Load() {
		t.Fatal("cleanup did not run")
	}
	if got := gen.State(); got != genStopped {
		t.Fatalf("generation state = %v, want stopped", got)
	}
	if tasks := gen.root.Snapshot(); len(tasks) != 0 {
		t.Fatalf("tasks left after the stop: %v", tasks)
	}
}

// A restart asked for while the services are still being built is honored once
// they are up.
func TestRestartRequestedWhileStartingIsHonored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var builds atomic.Int32
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		if builds.Add(1) == 1 {
			gen.RequestRestart()
		}
		return noop, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := h.runAsync(ctx, sup)
	h.nextGeneration()
	h.nextGeneration()
	cancel()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
}
