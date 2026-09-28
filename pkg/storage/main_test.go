package storage_test

import (
	"os"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// TestMain points the config singleton at a throwaway directory once, so the
// tests can run in parallel without each resetting process-global state.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "decypharr-storage-test-")
	if err != nil {
		panic(err)
	}
	config.SetConfigPath(dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
