package vfs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	dfsconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const testStopBudget = 5 * time.Second

// newTestGroup returns a root task group that the test stops, within a bounded
// time, when it ends.
func newTestGroup(t *testing.T) *task.Group {
	t.Helper()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), testStopBudget)
		defer cancel()
		if err := g.Stop(ctx); err != nil {
			t.Errorf("stop the test group: %v", err)
		}
	})
	return g
}

func newLifecycleCache(t *testing.T, g *task.Group) (*Cache, *bytesBackend) {
	t.Helper()
	backend := &bytesBackend{
		entry: &storage.Entry{
			InfoHash: "hash",
			Name:     "Movie",
			Files:    map[string]*storage.File{"f": {Name: "f", Size: 10}},
		},
		content: make([]byte, 10),
	}
	cfg := dfsconfig.DefaultFuseConfig()
	cfg.CacheDir = t.TempDir()
	cfg.CacheCleanupInterval = time.Hour
	cache, err := NewCache(g, backend, cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return cache, backend
}

func snapshotPaths(g *task.Group) []string {
	infos := g.Snapshot()
	paths := make([]string, 0, len(infos))
	for _, info := range infos {
		paths = append(paths, info.Path)
	}
	return paths
}

func hasTask(paths []string, name string) bool {
	for _, path := range paths {
		if strings.HasSuffix(path, ":"+name) {
			return true
		}
	}
	return false
}

func TestCacheLoopsAreTasks(t *testing.T) {
	t.Parallel()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	newLifecycleCache(t, g)

	paths := snapshotPaths(g)
	for _, name := range []string{"evict", "speed"} {
		if !hasTask(paths, name) {
			t.Errorf("task %q is not running: %v", name, paths)
		}
	}

	ctx, cancel := task.Detached(t.Context(), testStopBudget)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if left := snapshotPaths(g); len(left) != 0 {
		t.Errorf("tasks left after stop: %v", left)
	}
}

func TestGetItemAfterCloseRefused(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	cache, backend := newLifecycleCache(t, g)

	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	before := cache.pool.Stats().Buffers

	item, err := cache.GetItem(backend.entry, "f", 10)
	if !errors.Is(err, task.ErrStopped) {
		t.Fatalf("GetItem after Close = (%v, %v), want task.ErrStopped", item, err)
	}
	if after := cache.pool.Stats().Buffers; after != before {
		t.Errorf("pool buffers = %d after the refused GetItem, want %d", after, before)
	}
	if _, ok := cache.items.Load(buildCacheKey("hash", "f")); ok {
		t.Error("the refused GetItem left an item in the map")
	}
}
