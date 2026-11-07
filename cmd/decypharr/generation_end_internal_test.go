package decypharr

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
)

// A signal that arrives while a restart is draining stops the process: no
// next generation is built on a cancelled context.
func TestSignalDuringARestartDrainBuildsNoNextGeneration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		service(t, gen, "http", cancel) // the signal lands while the services stop
		return noop, nil
	})
	done := h.runAsync(ctx, sup)
	h.nextGeneration().RequestRestart()
	if err := awaitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if len(h.gens) != 0 {
		t.Fatal("a generation was built after the signal")
	}
	if got := h.loads.Load(); got != 2 {
		t.Fatalf("config loaded %d times, want 2 (start, validation): none after the signal", got)
	}
}

// The token is re-armed before the owner hears about the rejection, so a save
// that asks for a restart in reaction to it is not swallowed by the old token.
func TestRejectedRestartReArmsBeforeReporting(t *testing.T) {
	t.Parallel()
	gen := newGeneration(task.NewRoot(t.Context(), "generation", zerolog.Nop()), zerolog.Nop())
	gen.OnRestartRejected(func(error) { gen.RequestRestart() })
	gen.RequestRestart()
	gen.rejectRestart(errors.New("config.json does not load"))
	select {
	case <-gen.restartRequested():
	default:
		t.Fatal("a restart requested while the rejection was reported was lost")
	}
}
