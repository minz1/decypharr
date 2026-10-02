package qbit_test

import (
	"os"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// TestMain pins the global config to a temp dir: convertToQBitTorrentTorrent
// calls config.Get for folder-naming, which calls [os.Exit] when no config path
// is set.
func TestMain(m *testing.M) {
	configDir, err := os.MkdirTemp("", "decypharr-qbit-test-")
	if err != nil {
		panic(err)
	}

	config.SetConfigPath(configDir)
	code := m.Run()
	_ = os.RemoveAll(configDir)
	os.Exit(code)
}
