package decypharr

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// The HTTP API finds the DFS cache operations by type assertion on the mount
// manager the Manager holds, so whatever installMount hands over must keep
// them.
func TestInstalledDFSMountKeepsItsCacheOperations(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.Mount.Type = config.MountTypeDFS
	})
	installMount(mgr, store.Get(), logger.Discard())
	installed := mgr.MountManager()
	if _, ok := installed.(interface {
		CleanupCache() (map[string]any, error)
	}); !ok {
		t.Fatalf("%T lost CleanupCache", installed)
	}
	if _, ok := installed.(interface {
		PurgeCache() (map[string]any, error)
	}); !ok {
		t.Fatalf("%T lost PurgeCache", installed)
	}
}
