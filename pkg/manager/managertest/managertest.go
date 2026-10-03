// Package managertest builds isolated managers for tests in other packages.
package managertest

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

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

// New builds a Manager over Store(t, edit) and stops it when the test ends.
func New(t testing.TB, edit func(*config.Config)) (*manager.Manager, *config.Store) {
	t.Helper()
	store := Store(t, edit)
	mgr, err := manager.New(store, logger.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if stopErr := mgr.Stop(); stopErr != nil {
			t.Error(stopErr)
		}
	})
	return mgr, store
}
