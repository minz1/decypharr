package manager

import (
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

// testConfigStore loads a fresh installation in a temporary directory,
// applies edit (when not nil) and publishes it.
func testConfigStore(t *testing.T, edit func(*config.Config)) *config.Store {
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

// withTestConfig gives m a fresh default configuration and returns m.
func withTestConfig(t *testing.T, m *Manager) *Manager {
	t.Helper()
	m.store = testConfigStore(t, nil)
	m.config = m.store.Get()
	if m.arr == nil {
		m.arr = arr.New(m.store, nil, zerolog.Nop())
	}
	return m
}
