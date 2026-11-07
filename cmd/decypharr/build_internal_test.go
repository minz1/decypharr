package decypharr

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
)

// The real services come up under the generation's root, the generation
// serves once the manager is ready, and a stop drains the whole tree.
func TestBuildServicesRunsAndDrainsTheRealTree(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(t.TempDir(), config.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	cfg.BindAddress = "127.0.0.1"
	cfg.Port = "0"
	gens := make(chan *generation, 1)
	sup := supervisor{
		load: func() (*config.Config, error) { return cfg, nil },
		logs: func(*config.Config) *logger.Factory { return logger.Discard() },
		build: func(gen *generation, cfg *config.Config, logs *logger.Factory) (func(), error) {
			gens <- gen
			return buildServices(gen, cfg, logs)
		},
		drainDeadline: waitFor,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.run(ctx) }()

	gen := <-gens
	deadline := time.After(waitFor)
	for gen.State() != genServing {
		select {
		case <-deadline:
			t.Fatalf("generation is %v, want serving once the manager is ready", gen.State())
		case <-time.After(5 * time.Millisecond):
		}
	}
	var paths []string
	for _, info := range gen.root.Snapshot() {
		paths = append(paths, info.Path)
	}
	for _, want := range []string{"generation/manager:run", "generation/http:serve", "generation/mount:stop"} {
		if !slices.Contains(paths, want) {
			t.Errorf("tasks %v lack %s", paths, want)
		}
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(waitFor):
		t.Fatal("the generation did not drain")
	}
	if left := gen.root.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after the stop: %v", left)
	}
}
