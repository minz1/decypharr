//go:build linux || (darwin && amd64)

package hanwen

import (
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

func newTestDir(level DirLevel) *Dir {
	log := zerolog.Nop()
	return NewDir(nil, "", level, 0, &config.FuseConfig{}, log, logger.NewRateLimitedLogger(logger.WithLogger(log)))
}

// go-fuse calls the root's OnForget from the bridge's OnUnmount once the serve
// loops have exited; the handle turns that into one closed Done channel.
func TestRootForgetEndsServe(t *testing.T) {
	t.Parallel()
	root := newTestDir(LevelRoot)
	calls := 0
	h := newMounted(nil, "", nil)
	root.onServeEnd = func() {
		calls++
		h.end()
	}

	raw := fs.NewNodeFS(root, &fs.Options{})
	select {
	case <-h.Done():
		t.Fatal("Done closed before the serve loop ended")
	default:
	}

	raw.OnUnmount()
	raw.OnUnmount()

	select {
	case <-h.Done():
	default:
		t.Fatal("Done not closed after OnUnmount")
	}
	if calls == 0 {
		t.Fatal("root hook never ran")
	}
}

func TestNonRootForgetIsQuiet(t *testing.T) {
	t.Parallel()
	d := newTestDir(LevelTorrent)
	var f fs.NodeOnForgetter = d
	f.OnForget()
}
