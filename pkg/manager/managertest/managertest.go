// Package managertest builds isolated managers for tests in other packages.
package managertest

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

// stopTimeout bounds the stop of a test manager.
const stopTimeout = 30 * time.Second

// Store loads a fresh installation in a temporary directory, applies edit
// (when not nil) and publishes the result. The environment is ignored.
func Store(t testing.TB, edit func(*config.Config)) *config.Store {
	t.Helper()
	cfg, err := config.Load(t.TempDir(), config.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(cfg)
	}
	return config.NewStore(cfg)
}

// New builds a Manager over Store(t, edit), on a task root of its own, and
// stops it when the test ends.
func New(t testing.TB, edit func(*config.Config)) (*manager.Manager, *config.Store) {
	t.Helper()
	store := Store(t, edit)
	root := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
	mgr, err := manager.New(root.Child("manager"), store, logger.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), stopTimeout)
		defer cancel()
		if stopErr := root.Stop(ctx); stopErr != nil {
			t.Error(stopErr)
		}
	})
	return mgr, store
}
