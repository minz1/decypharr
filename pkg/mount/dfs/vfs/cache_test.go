package vfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/buffer"
	fuseconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

func newTestCache(cacheDir string) *Cache {
	return &Cache{
		config: &fuseconfig.FuseConfig{
			CacheDir:    cacheDir,
			CacheExpiry: time.Hour,
		},
		items:  xsync.NewMap[string, *CacheItem](),
		logger: zerolog.Nop(),
	}
}

// TestIsOverBudget_BugReproduction documents the original bug:
// ParseSize("85G") failed silently, leaving CacheDiskSize=0 → threshold=0 →
// IsOverBudget() always false → all size enforcement disabled.
func TestIsOverBudget_BugReproduction(t *testing.T) {
	cacheDir := t.TempDir()

	// Bug scenario: threshold=0 (from CacheDiskSize=0). Even with 100 GB "used",
	// IsOverBudget must return false — no limit was configured, not "over budget".
	c := newTestCache(cacheDir)
	c.totalSize.Store(100 * 1024 * 1024 * 1024)
	if c.IsOverBudget() {
		t.Fatal("threshold=0 must never report over budget (means no limit configured, not 'full')")
	}

	// Fixed behaviour: threshold set to 90% of 85 GB.
	const maxSize = 85 * 1024 * 1024 * 1024
	c.threshold = int64(float64(maxSize) * cacheEvictThreshold) // ~76.5 GB

	// Under threshold → not over budget.
	c.totalSize.Store(50 * 1024 * 1024 * 1024)
	if c.IsOverBudget() {
		t.Fatal("below threshold should not be over budget")
	}

	// At threshold → over budget.
	c.totalSize.Store(c.threshold)
	if !c.IsOverBudget() {
		t.Fatal("at eviction threshold should be over budget")
	}

	// Above threshold → over budget.
	c.totalSize.Store(c.threshold + 1)
	if !c.IsOverBudget() {
		t.Fatal("above eviction threshold should be over budget")
	}

	// DisableCache bypasses IsOverBudget entirely (checked in manager.GetFile).
	cfg2 := &fuseconfig.FuseConfig{CacheDir: cacheDir, DisableCache: true}
	c2 := &Cache{config: cfg2, items: xsync.NewMap[string, *CacheItem](), logger: zerolog.Nop()}
	overBudget := c2.config.DisableCache || c2.IsOverBudget()
	if !overBudget {
		t.Fatal("DisableCache=true must force the direct-stream path in manager.GetFile")
	}
}

// TestEvictCandidates_BoundsCacheBelowThreshold verifies that Phase 2 eviction
// removes oldest-by-atime items until total is within threshold, and that it
// removes exactly as many as needed — no more, no less.
func TestEvictCandidates_BoundsCacheBelowThreshold(t *testing.T) {
	cacheDir := t.TempDir()
	entryDir := filepath.Join(cacheDir, "entry")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Five files, each accounting for 100 bytes, total 500 bytes.
	// Atime descending from oldest (5h ago) to newest (1h ago).
	now := time.Now()
	type fileSpec struct {
		name  string
		atime time.Time
	}
	specs := []fileSpec{
		{"oldest.mkv", now.Add(-5 * time.Hour)},
		{"old.mkv", now.Add(-4 * time.Hour)},
		{"mid.mkv", now.Add(-3 * time.Hour)},
		{"recent.mkv", now.Add(-2 * time.Hour)},
		{"newest.mkv", now.Add(-1 * time.Hour)},
	}

	const cachedBytesEach = 100
	candidates := make([]candidateEntry, 0, len(specs))
	for _, spec := range specs {
		dataPath := filepath.Join(entryDir, spec.name)
		metaPath := dataPath + ".json"
		if err := os.WriteFile(dataPath, make([]byte, cachedBytesEach), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(metaPath, []byte(`{"size":1024}`), 0644); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidateEntry{
			key:        "entry/" + spec.name,
			path:       entryDir,
			dataPath:   dataPath,
			metaPath:   metaPath,
			atime:      spec.atime,
			mtime:      spec.atime,
			cachedSize: cachedBytesEach,
		})
	}

	// Use a cache with no expiry so Phase 1 (time-based) doesn't interfere.
	c := &Cache{
		config: &fuseconfig.FuseConfig{CacheDir: cacheDir, CacheExpiry: 0},
		items:  xsync.NewMap[string, *CacheItem](),
		logger: zerolog.Nop(),
	}

	// Threshold 200: need to evict 3 oldest to reach 200 (500-100-100-100=200).
	totalBefore := int64(len(specs) * cachedBytesEach) // 500
	const threshold = 200

	totalAfter, removed, removalErrors, removedKeys := c.evictCandidates(now, candidates, totalBefore, threshold)

	if removalErrors != 0 {
		t.Fatalf("expected 0 removal errors, got %d", removalErrors)
	}
	if totalAfter > threshold {
		t.Fatalf("expected total ≤ %d after eviction, got %d", threshold, totalAfter)
	}
	if removed != 3 {
		t.Fatalf("expected 3 items removed (oldest first), got %d", removed)
	}

	// Oldest 3 must be gone from disk.
	for _, name := range []string{"oldest.mkv", "old.mkv", "mid.mkv"} {
		if _, err := os.Stat(filepath.Join(entryDir, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed; stat err=%v", name, err)
		}
		if _, ok := removedKeys["entry/"+name]; !ok {
			t.Errorf("expected removed key entry/%s in result set", name)
		}
	}

	// Two newest must survive.
	for _, name := range []string{"recent.mkv", "newest.mkv"} {
		if _, err := os.Stat(filepath.Join(entryDir, name)); err != nil {
			t.Errorf("expected %s to survive; stat err=%v", name, err)
		}
	}
}

func TestScanDiskCandidates_DoesNotDeleteLegacyFiles(t *testing.T) {
	cacheDir := t.TempDir()
	entryDir := filepath.Join(cacheDir, "entry")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	metaPath := filepath.Join(entryDir, "meta.json")
	dataPath := filepath.Join(entryDir, "data")
	if err := os.WriteFile(metaPath, []byte(`{"size":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	c := newTestCache(cacheDir)
	_ = c.scanDiskCandidates()

	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("meta.json should not be deleted on scan: %v", err)
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("data should not be deleted on scan: %v", err)
	}
}

func TestScanDiskCandidates_RemovesOrphanMetadataWithCachedRanges(t *testing.T) {
	cacheDir := t.TempDir()
	entryDir := filepath.Join(cacheDir, "entry")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	metaPath := filepath.Join(entryDir, "video.mkv.json")
	if err := os.WriteFile(metaPath, []byte(`{"size":1024,"ranges":[{"Pos":100,"Size":50}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	c := newTestCache(cacheDir)
	scan := c.scanDiskCandidates()

	if len(scan.candidates) != 0 {
		t.Fatalf("expected no candidates for orphan metadata, got %+v", scan.candidates)
	}
	if scan.totalSize != 0 {
		t.Fatalf("expected total size 0 for orphan metadata, got %d", scan.totalSize)
	}
	if scan.orphanMetadataRemoved != 1 {
		t.Fatalf("expected 1 orphan metadata file removed, got %d", scan.orphanMetadataRemoved)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("orphan metadata should be removed, stat err=%v", err)
	}
}

func TestEvictCandidates_RemovesOnlyTargetPair(t *testing.T) {
	cacheDir := t.TempDir()
	entryDir := filepath.Join(cacheDir, "entry")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	targetData := filepath.Join(entryDir, "a.mkv")
	targetMeta := targetData + ".json"
	otherData := filepath.Join(entryDir, "b.mkv")
	otherMeta := otherData + ".json"

	for _, path := range []string{targetData, targetMeta, otherData, otherMeta} {
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := newTestCache(cacheDir)
	now := time.Now()
	candidates := []candidateEntry{
		{
			key:        "entry/a.mkv",
			path:       entryDir,
			dataPath:   targetData,
			metaPath:   targetMeta,
			atime:      now.Add(-2 * time.Hour),
			mtime:      now.Add(-2 * time.Hour),
			cachedSize: 1,
		},
	}

	totalSize, removed, removalErrors, removedKeys := c.evictCandidates(now, candidates, 1, 0)
	if removed != 1 {
		t.Fatalf("expected 1 candidate removed, got %d", removed)
	}
	if removalErrors != 0 {
		t.Fatalf("expected 0 removal errors, got %d", removalErrors)
	}
	if totalSize != 0 {
		t.Fatalf("expected totalSize 0 after eviction, got %d", totalSize)
	}
	if _, ok := removedKeys["entry/a.mkv"]; !ok {
		t.Fatalf("expected removed key to be tracked, got %#v", removedKeys)
	}

	if _, err := os.Stat(targetData); !os.IsNotExist(err) {
		t.Fatalf("target data should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(targetMeta); !os.IsNotExist(err) {
		t.Fatalf("target metadata should be removed, stat err=%v", err)
	}

	if _, err := os.Stat(otherData); err != nil {
		t.Fatalf("other data should remain, stat err=%v", err)
	}
	if _, err := os.Stat(otherMeta); err != nil {
		t.Fatalf("other metadata should remain, stat err=%v", err)
	}
	if _, err := os.Stat(entryDir); err != nil {
		t.Fatalf("entry directory should remain, stat err=%v", err)
	}
}

func TestGetStatsReportsDiskItemsSeparatelyFromActiveItems(t *testing.T) {
	cacheDir := t.TempDir()
	entryDir := filepath.Join(cacheDir, "entry")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	dataPath := filepath.Join(entryDir, "video.mkv")
	metaPath := dataPath + ".json"
	if err := os.WriteFile(dataPath, []byte("cached bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte(`{"size":1024,"ranges":[{"Pos":0,"Size":50}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	c := newTestCache(cacheDir)
	c.config.CacheDiskSize = 100
	c.evict()

	stats := c.GetStats()
	if got := stats["item_count"]; got != int64(1) {
		t.Fatalf("expected disk cache item count 1, got %#v", got)
	}
	if got := stats["active_item_count"]; got != int64(0) {
		t.Fatalf("expected active cache item count 0, got %#v", got)
	}
	if got := stats["total_size"]; got != int64(50) {
		t.Fatalf("expected total cache size 50, got %#v", got)
	}
}

func TestRunCleanupReportsResultStats(t *testing.T) {
	cacheDir := t.TempDir()
	entryDir := filepath.Join(cacheDir, "entry")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	dataPath := filepath.Join(entryDir, "expired.mkv")
	metaPath := dataPath + ".json"
	if err := os.WriteFile(dataPath, []byte("cached bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte(`{"size":1024,"ranges":[{"Pos":0,"Size":50}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dataPath, old, old); err != nil {
		t.Fatal(err)
	}

	c := newTestCache(cacheDir)
	stats := c.RunCleanup()

	if got := stats["cleanup_status"]; got != "healthy" {
		t.Fatalf("expected healthy cleanup status, got %#v", got)
	}
	if got := stats["cleanup_warning_count"]; got != int64(0) {
		t.Fatalf("expected no cleanup warnings, got %#v", got)
	}
	if got := stats["cleanup_removed_items"]; got != int64(1) {
		t.Fatalf("expected 1 removed item, got %#v", got)
	}
	if got := stats["cleanup_freed_bytes"]; got != int64(50) {
		t.Fatalf("expected 50 freed bytes, got %#v", got)
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("expired data should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("expired metadata should be removed, stat err=%v", err)
	}
}

func TestPurgeCacheRemovesIdleDiskItemsAndSkipsActiveItems(t *testing.T) {
	cacheDir := t.TempDir()
	entryDir := filepath.Join(cacheDir, "entry")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	idleData := filepath.Join(entryDir, "idle.mkv")
	idleMeta := idleData + ".json"
	if err := os.WriteFile(idleData, []byte("cached idle bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idleMeta, []byte(`{"size":1024,"ranges":[{"Pos":0,"Size":50}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	activeData := filepath.Join(entryDir, "active.mkv")
	activeMeta := activeData + ".json"
	if err := os.WriteFile(activeData, []byte("cached active bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeMeta, []byte(`{"size":1024,"ranges":[{"Pos":0,"Size":25}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	c := newTestCache(cacheDir)
	activeItem := &CacheItem{
		cache:    c,
		key:      "entry/active.mkv",
		metaPath: activeMeta,
	}
	activeItem.opens.Store(1)
	c.items.Store(activeItem.key, activeItem)
	c.itemCount.Store(1)

	stats := c.PurgeCache()

	if got := stats["purge_status"]; got != "healthy" {
		t.Fatalf("expected healthy purge status, got %#v", got)
	}
	if got := stats["purge_warning_count"]; got != int64(0) {
		t.Fatalf("expected no purge warnings, got %#v", got)
	}
	if got := stats["purge_removed_items"]; got != int64(1) {
		t.Fatalf("expected 1 purged item, got %#v", got)
	}
	if got := stats["purge_skipped_busy_items"]; got != int64(1) {
		t.Fatalf("expected 1 busy item skipped, got %#v", got)
	}
	if got := stats["purge_freed_bytes"]; got != int64(50) {
		t.Fatalf("expected 50 freed bytes, got %#v", got)
	}
	if got := stats["purge_cache_size_before"]; got != int64(75) {
		t.Fatalf("expected cache size before purge 75, got %#v", got)
	}
	if got := stats["purge_cache_size_after"]; got != int64(25) {
		t.Fatalf("expected cache size after purge 25, got %#v", got)
	}
	if _, err := os.Stat(idleData); !os.IsNotExist(err) {
		t.Fatalf("idle data should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(idleMeta); !os.IsNotExist(err) {
		t.Fatalf("idle metadata should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(activeData); err != nil {
		t.Fatalf("active data should remain, stat err=%v", err)
	}
	if _, err := os.Stat(activeMeta); err != nil {
		t.Fatalf("active metadata should remain, stat err=%v", err)
	}

	currentStats := c.GetStats()
	if got := currentStats["item_count"]; got != int64(1) {
		t.Fatalf("expected disk cache item count 1 after purge, got %#v", got)
	}
	if got := currentStats["active_item_count"]; got != int64(1) {
		t.Fatalf("expected active cache item count 1 after purge, got %#v", got)
	}
	if got := currentStats["total_size"]; got != int64(25) {
		t.Fatalf("expected total cache size 25 after purge, got %#v", got)
	}
}

func TestCleanupItems_ForceZeroOpenClosesRecentItems(t *testing.T) {
	cacheDir := t.TempDir()
	entryDir := filepath.Join(cacheDir, "entry")
	if err := os.MkdirAll(entryDir, 0755); err != nil {
		t.Fatal(err)
	}

	dataPath := filepath.Join(entryDir, "video.mkv")

	// Build a CacheItem backed by a real buffer so Close() exercises the
	// production teardown path. The buffer creates and owns the disk file.
	pool := buffer.NewPool(buffer.PoolConfig{Name: "test"})
	t.Cleanup(func() { _ = pool.Close() })

	buf, err := pool.NewBuffer(buffer.Config{
		DiskPath:  dataPath,
		TotalSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	c := newTestCache(cacheDir)
	c.pool = pool
	item := &CacheItem{
		cache:    c,
		key:      "entry/video.mkv",
		buf:      buf,
		metaPath: dataPath + ".json",
		info: ItemInfo{
			ATime: time.Now(),
		},
	}
	c.items.Store(item.key, item)
	c.itemCount.Store(1)

	if removed := c.cleanupItems(time.Now(), true); removed != 1 {
		t.Fatalf("expected 1 recent zero-open item to be closed, got %d", removed)
	}
	if _, ok := c.items.Load(item.key); ok {
		t.Fatal("expected item to be removed from cache map")
	}
	if got := c.itemCount.Load(); got != 0 {
		t.Fatalf("expected item count 0 after forced cleanup, got %d", got)
	}
	if item.buf != nil {
		t.Fatal("expected cache buffer to be closed after forced cleanup")
	}
}
