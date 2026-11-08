package vfs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	dfsconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const testStopBudget = 5 * time.Second

// newTestGroup returns a root task group that the test stops, within a bounded
// time, when it ends.
func newTestGroup(t testing.TB) *task.Group {
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

// stopItemGroup stops the item's tasks, the metadata writer's last flush
// included.
func stopItemGroup(t testing.TB, item *CacheItem) {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), testStopBudget)
	defer cancel()
	if err := item.group.Stop(ctx); err != nil {
		t.Fatalf("stop the item's group: %v", err)
	}
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

func TestItemTasksEndWithGroup(t *testing.T) {
	t.Parallel()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	cache, backend := newLifecycleCache(t, g)
	t.Cleanup(func() { _ = cache.Close() })

	if _, err := cache.GetItem(backend.entry, "f", 10); err != nil {
		t.Fatal(err)
	}
	paths := snapshotPaths(g)
	for _, want := range []string{"test/items/1:meta", "test/items/1:kicker"} {
		if !slices.Contains(paths, want) {
			t.Errorf("task %q is not running: %v", want, paths)
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

func TestStopFlushesDirtyMetadata(t *testing.T) {
	t.Parallel()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	cache, backend := newLifecycleCache(t, g)
	t.Cleanup(func() { _ = cache.Close() })

	item, err := cache.GetItem(backend.entry, "f", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, writeErr := item.WriteAtNoOverwrite([]byte("0123456789"), 0); writeErr != nil {
		t.Fatal(writeErr)
	}
	if flushErr := item.buf.Flush(); flushErr != nil {
		t.Fatal(flushErr)
	}
	want := item.buf.PersistedRanges(0, 10)
	if len(want) == 0 {
		t.Fatal("test setup: nothing reached the disk")
	}

	ctx, cancel := task.Detached(t.Context(), testStopBudget)
	defer cancel()
	if stopErr := g.Stop(ctx); stopErr != nil {
		t.Fatalf("stop: %v", stopErr)
	}
	assertMetaRanges(t, item, len(want))

	if closeErr := cache.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	assertMetaRanges(t, item, len(want))
}

func assertMetaRanges(t *testing.T, item *CacheItem, want int) {
	t.Helper()
	var info ItemInfo
	if err := decodeJSONFile(item.metaPath, &info); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if len(info.Rs) != want {
		t.Errorf("metadata holds %d ranges, want %d: %+v", len(info.Rs), want, info.Rs)
	}
}

// A kicker the item's group refuses leaves the session idle with a finished
// sentinel, so tearing the session down does not wait for a goroutine that
// never ran.
func TestKickerRefusedDoesNotHang(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := task.NewRoot(t.Context(), "test", zerolog.Nop())
		g.Close()
		synctest.Wait()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		dls := &Downloaders{group: g, parentCtx: ctx, ctx: ctx, cancel: cancel, idle: true, item: &CacheItem{}}

		dls.mu.Lock()
		dls.restartKickerIfIdleLocked()
		idle := dls.idle
		dls.mu.Unlock()
		if !idle {
			t.Error("a refused kicker left the session marked busy")
		}

		returned := make(chan struct{})
		go func() {
			defer close(returned)
			dls.StopAll()
		}()
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Fatal("StopAll is waiting for a kicker that never started")
		}
	})
}
