package buffer

import (
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestDiskBackstop_ReclaimsOverLimit verifies that reclaimDisk punches holes
// behind the read head and brings diskInUse back under DiskLimit. This is the
// pool-level bound that keeps a single open stream from exhausting the cache
// partition even when whole-file eviction can't help (the item is still open).
func TestDiskBackstop_ReclaimsOverLimit(t *testing.T) {
	dir := t.TempDir()

	const diskLimit = 512
	pool := NewPool(PoolConfig{
		Name:       "test",
		DiskLimit:  diskLimit,
		BackWindow: 0, // punch everything behind the read head
	})
	defer pool.Close()

	// MemorySize: 0 forces write-through so data hits disk (and the range
	// tracker) immediately, without waiting for a block flush.
	buf, err := pool.NewBuffer(Config{
		DiskPath:   filepath.Join(dir, "stream.buf"),
		TotalSize:  1024,
		MemorySize: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024)
	if _, err := buf.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	if got := pool.diskInUse.Load(); got == 0 {
		t.Fatal("diskInUse should be > 0 after write")
	}

	// Advance the read head past all data so everything is below the punch
	// window (backWindow=0 means ceiling = readHead, so all data is eligible).
	buf.SetReadHead(1024)

	pool.reclaimDisk()

	stats := pool.Stats()
	if stats.DiskInUse > diskLimit {
		t.Fatalf("DiskInUse=%d after backstop, want ≤ %d", stats.DiskInUse, diskLimit)
	}
	if stats.DiskPunches == 0 {
		t.Fatal("expected at least one disk punch")
	}
}

// TestDiskBackstop_FiresOnEvictCallback verifies that the onEvict hook wired
// via Config.OnEvict is called for every punched range. This is the bridge
// between the pool backstop and the vfs cache's totalSize counter: without the
// callback, totalSize drifts upward and IsOverBudget() stays false.
func TestDiskBackstop_FiresOnEvictCallback(t *testing.T) {
	dir := t.TempDir()

	pool := NewPool(PoolConfig{
		Name:       "test",
		DiskLimit:  512,
		BackWindow: 0,
	})
	defer pool.Close()

	var evictCalls atomic.Int32
	var evictedBytes atomic.Int64

	buf, err := pool.NewBuffer(Config{
		DiskPath:   filepath.Join(dir, "stream.buf"),
		TotalSize:  1024,
		MemorySize: 0,
		OnEvict: func(off, length int64) {
			evictCalls.Add(1)
			evictedBytes.Add(length)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024)
	if _, err := buf.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	buf.SetReadHead(1024)
	pool.reclaimDisk()

	if evictCalls.Load() == 0 {
		t.Fatal("OnEvict callback was never called")
	}
	if evictedBytes.Load() == 0 {
		t.Fatal("OnEvict reported zero bytes evicted")
	}
}

// TestDiskBackstop_NoOpWithoutReadHead verifies that the backstop does not
// punch data when no read head has been set (head=0). This protects write
// patterns that haven't started streaming yet.
func TestDiskBackstop_NoOpWithoutReadHead(t *testing.T) {
	dir := t.TempDir()

	pool := NewPool(PoolConfig{
		Name:       "test",
		DiskLimit:  1, // so tiny that any data would trigger eviction
		BackWindow: 0,
	})
	defer pool.Close()

	buf, err := pool.NewBuffer(Config{
		DiskPath:   filepath.Join(dir, "stream.buf"),
		TotalSize:  1024,
		MemorySize: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 512)
	if _, err := buf.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	diskBefore := pool.diskInUse.Load()

	// No SetReadHead call — backstop must be a no-op.
	pool.reclaimDisk()

	if got := pool.diskInUse.Load(); got != diskBefore {
		t.Fatalf("diskInUse changed without a read head: before=%d after=%d", diskBefore, got)
	}
	if pool.Stats().DiskPunches != 0 {
		t.Fatal("expected no punches without a read head")
	}
}
