package decypharr

import (
	"context"
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
)

// A service that fails after a restart was picked, but before the drain begins, still fails the generation: the
// process exits non-zero instead of restarting. The failure lands while the supervisor validates the next
// configuration, so the restart is always the first thing it sees.
func TestFailureBeatsARestartPickedFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	failure := errors.New("serve: listener died")
	fail := make(chan struct{})
	var built *generation
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		built = gen
		err := gen.root.Child("http").GoCritical("serve", func(ctx context.Context) error {
			select {
			case <-fail:
				return failure
			case <-ctx.Done():
				return nil
			}
		})
		return noop, err
	})
	load := sup.load
	sup.load = func() (*config.Config, error) {
		cfg, err := load()
		if h.loads.Load() == 2 { // the validation of the restart request
			close(fail)
			<-built.root.Failed()
		}
		return cfg, err
	}
	done := h.runAsync(t.Context(), sup)
	h.nextGeneration().RequestRestart()

	err := awaitRun(t, done)
	if !errors.Is(err, failure) {
		t.Fatalf("run returned %v, want the service failure", err)
	}
	if len(h.gens) != 0 {
		t.Fatal("a generation was built after the failed one")
	}
}
