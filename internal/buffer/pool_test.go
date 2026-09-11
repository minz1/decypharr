package buffer

import (
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestDiskBackstop_ReclaimsOverLimit verifies that reclaimDiskTo punches holes
// behind the read head and brings diskInUse back under DiskLimit. This is the
// pool-level bound that keeps a single open stream from exhausting the cache
// partition even when whole-file eviction can't help (the item is still open).
func TestDiskBackstop_ReclaimsOverLimit(t *testing.T) {
	dir := t.TempDir()

	// Start unlimited so the write below isn't rejected by the reservation
	// gate (reserveDisk has nothing to reclaim from yet, since nothing has
	// hit disk). The limit is dropped below after the data is safely on disk
	// to simulate the pool budget shrinking out from under an open stream.
	pool := NewPool(PoolConfig{
		Name:       "test",
		BackWindow: 0, // punch everything behind the read head
	})
	defer pool.Close()

	buf, err := pool.NewBuffer(Config{
		DiskPath:  filepath.Join(dir, "stream.buf"),
		TotalSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 1024)
	if _, err := buf.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	// Force the write out of the RAM window and onto disk (and the range
	// tracker) immediately, rather than waiting for a block flush.
	if err := buf.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := pool.diskInUse.Load(); got == 0 {
		t.Fatal("diskInUse should be > 0 after write")
	}

	// Advance the read head past all data so everything is below the punch
	// window (backWindow=0 means ceiling = readHead, so all data is eligible).
	buf.SetReadHead(1024)

	const diskLimit = 512
	pool.diskLimit.Store(diskLimit)
	pool.reclaimDiskTo(diskLimit)

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
		BackWindow: 0,
	})
	defer pool.Close()

	var evictCalls atomic.Int32
	var evictedBytes atomic.Int64

	buf, err := pool.NewBuffer(Config{
		DiskPath:  filepath.Join(dir, "stream.buf"),
		TotalSize: 1024,
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
	if err := buf.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	buf.SetReadHead(1024)

	const diskLimit = 512
	pool.diskLimit.Store(diskLimit)
	pool.reclaimDiskTo(diskLimit)

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
		BackWindow: 0,
	})
	defer pool.Close()

	buf, err := pool.NewBuffer(Config{
		DiskPath:  filepath.Join(dir, "stream.buf"),
		TotalSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 512)
	if _, err := buf.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := buf.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	diskBefore := pool.diskInUse.Load()
	if diskBefore == 0 {
		t.Fatal("diskInUse should be > 0 after write, to make the no-op assertion below meaningful")
	}

	// So tiny that any data would trigger eviction if the missing read head
	// didn't suppress it.
	const diskLimit = 1
	pool.diskLimit.Store(diskLimit)

	// No SetReadHead call — backstop must be a no-op.
	pool.reclaimDiskTo(diskLimit)

	if got := pool.diskInUse.Load(); got != diskBefore {
		t.Fatalf("diskInUse changed without a read head: before=%d after=%d", diskBefore, got)
	}
	if pool.Stats().DiskPunches != 0 {
		t.Fatal("expected no punches without a read head")
	}
}
