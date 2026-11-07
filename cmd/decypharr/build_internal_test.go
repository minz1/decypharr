package decypharr

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
)

// realRun runs the real services (manager, http) under a supervisor. Every
// generation is sent on gens; load answers each call with the same config.
type realRun struct {
	t     *testing.T
	gens  chan *generation
	loads atomic.Int32
	done  chan error
	sup   supervisor
}

func startReal(ctx context.Context, t *testing.T, onLoad func(call int32)) *realRun {
	t.Helper()
	cfg, err := config.Load(t.TempDir(), config.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg.BindAddress = "127.0.0.1"
	cfg.Port = "0"
	run := &realRun{t: t, gens: make(chan *generation, 4), done: make(chan error, 1)}
	run.sup = supervisor{
		load: func() (*config.Config, error) {
			call := run.loads.Add(1)
			if onLoad != nil {
				onLoad(call)
			}
			return cfg, nil
		},
		logs: func(*config.Config) *logger.Factory { return logger.Discard() },
		build: func(gen *generation, cfg *config.Config, logs *logger.Factory) (func(), error) {
			run.gens <- gen
			return buildServices(gen, cfg, logs)
		},
		drainDeadline: waitFor,
	}
	go func() { run.done <- run.sup.run(ctx) }()
	return run
}

func (r *realRun) generation() *generation {
	r.t.Helper()
	select {
	case gen := <-r.gens:
		return gen
	case <-time.After(waitFor):
		r.t.Fatal("no generation was built")
		return nil
	}
}

func (r *realRun) finished() error {
	r.t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(waitFor):
		r.t.Fatal("the supervisor did not return")
		return nil
	}
}

func waitServing(t *testing.T, gen *generation) {
	t.Helper()
	deadline := time.After(waitFor)
	for gen.State() != genServing {
		select {
		case <-deadline:
			t.Fatalf("generation is %v, want serving once the manager is ready", gen.State())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func taskPaths(gen *generation) []string {
	var paths []string
	for _, info := range gen.root.Snapshot() {
		paths = append(paths, info.Path)
	}
	return paths
}

// The real services come up under the generation's root, the generation
// serves once the manager is ready, and a stop drains the whole tree.
func TestBuildServicesRunsAndDrainsTheRealTree(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := startReal(ctx, t, nil)
	gen := run.generation()
	waitServing(t, gen)
	paths := taskPaths(gen)
	for _, want := range []string{"generation/manager:storage", "generation/http:serve"} {
		if !slices.Contains(paths, want) {
			t.Errorf("tasks %v lack %s", paths, want)
		}
	}
	cancel()
	if err := run.finished(); err != nil {
		t.Fatal(err)
	}
	if left := taskPaths(gen); len(left) != 0 {
		t.Fatalf("tasks left after the stop: %v", left)
	}
}

// On a signal the server finishes before the manager goes away: a signal must
// not cancel the manager while requests that use it are still being served.
func TestRealServicesStopServerBeforeManager(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := startReal(ctx, t, nil)
	gen := run.generation()
	waitServing(t, gen)

	var violation string
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			paths := taskPaths(gen)
			managerGone := !slices.Contains(paths, "generation/manager:storage")
			serverRuns := slices.ContainsFunc(paths, func(path string) bool {
				return strings.HasPrefix(path, "generation/http:")
			})
			if managerGone && serverRuns {
				violation = strings.Join(paths, ", ")
				return
			}
			if len(paths) == 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	cancel()
	if err := run.finished(); err != nil {
		t.Fatal(err)
	}
	<-sampled
	if violation != "" {
		t.Fatalf("the manager stopped while the server still ran: %s", violation)
	}
}

// A stop that arrives before the manager is ready drains the real tree
// cleanly and returns nil.
func TestRealServicesStopBeforeReady(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := startReal(ctx, t, nil)
	gen := run.generation()
	cancel() // possibly before the manager is ready
	if err := run.finished(); err != nil {
		t.Fatal(err)
	}
	if got := gen.State(); got != genStopped {
		t.Fatalf("generation is %v, want stopped", got)
	}
	if left := taskPaths(gen); len(left) != 0 {
		t.Fatalf("tasks left after the stop: %v", left)
	}
}

// A signal that arrives while a restart drains the real tree stops the
// process instead of building another generation.
func TestRealServicesSignalDuringRestartDrain(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := startReal(ctx, t, func(call int32) {
		if call == 2 { // the validation of the restart, just before the drain
			cancel()
		}
	})
	gen := run.generation()
	waitServing(t, gen)
	gen.RequestRestart()
	if err := run.finished(); err != nil {
		t.Fatal(err)
	}
	if len(run.gens) != 0 {
		t.Fatal("a generation was built after the signal")
	}
	if got := gen.State(); got != genStopped {
		t.Fatalf("generation is %v, want stopped", got)
	}
}
