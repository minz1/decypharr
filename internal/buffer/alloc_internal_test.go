package buffer

import "testing"

func TestPoolCountsReusableAllocations(t *testing.T) {
	t.Parallel()
	p := newTestPool(t, PoolConfig{})
	b := newTestBuffer(t, p, Config{MemorySize: 4 * blockSize})
	data := make([]byte, 4*blockSize)
	mustWrite(t, b, data, 0)
	mustDiscard(t, b, 0, int64(len(data)))
	stats := p.Stats()
	if stats.MemoryInUse != 0 || stats.MemoryAllocated != int64(len(data)) {
		t.Fatalf("discarded blocks must remain charged for reuse: %+v", stats)
	}
	mustWrite(t, b, data, 0)
	stats = p.Stats()
	if stats.MemoryInUse != int64(len(data)) || stats.MemoryAllocated != int64(len(data)) {
		t.Fatalf("reuse must not charge an allocation twice: %+v", stats)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if stats = p.Stats(); stats.MemoryInUse != 0 || stats.MemoryAllocated != 0 {
		t.Fatalf("close left charged blocks: %+v", stats)
	}
}

func TestPoolPressureReleasesReuseBeforeActiveData(t *testing.T) {
	t.Parallel()
	p := newTestPool(t, PoolConfig{MemoryBudget: 4 * blockSize})
	idle := newTestBuffer(t, p, Config{MemorySize: 4 * blockSize})
	active := newTestBuffer(t, p, Config{MemorySize: 4 * blockSize})
	data := make([]byte, 4*blockSize)
	mustWrite(t, idle, data, 0)
	mustDiscard(t, idle, 0, int64(len(data)))
	if got := p.Stats().MemoryAllocated; got != int64(len(data)) {
		t.Fatalf("idle buffer allocations = %d, want %d", got, len(data))
	}
	mustWrite(t, active, []byte{42}, 0)
	waitFor(t, "reuse released under pool pressure", func() bool {
		return p.Stats().MemoryAllocated == blockSize
	})
	var got [1]byte
	if _, err := active.ReadAt(got[:], 0); err != nil || got[0] != 42 {
		t.Fatalf("active data was removed: data=%v err=%v", got, err)
	}
	if got := active.Stats().Evictions; got != 0 {
		t.Fatalf("evicted active blocks while reuse was available: %d", got)
	}
}

func TestAdmissionPreservesActiveDataAtAllocationLimit(t *testing.T) {
	t.Parallel()
	for _, reuseOwner := range []string{"idle", "active"} {
		t.Run(reuseOwner, func(t *testing.T) {
			t.Parallel()
			testAdmissionPreservesActiveData(t, reuseOwner)
		})
	}
}

func testAdmissionPreservesActiveData(t *testing.T, reuseOwner string) {
	t.Helper()
	p := newTestPool(t, PoolConfig{MemoryBudget: 4 * blockSize})
	active := newTestBuffer(t, p, Config{MemorySize: 4 * blockSize})
	idle := newTestBuffer(t, p, Config{MemorySize: 4 * blockSize})
	for i := range 3 {
		mustWrite(t, active, []byte{42}, int64(i*blockSize))
	}
	owner := idle
	if reuseOwner == "active" {
		owner = active
	}
	mustWrite(t, owner, []byte{42}, 3*blockSize)
	mustDiscard(t, owner, 3*blockSize, blockSize)
	if stats := p.Stats(); stats.MemoryInUse != 3*blockSize || stats.MemoryAllocated != 4*blockSize {
		t.Fatalf("expected three active blocks and one reusable block: %+v", stats)
	}
	mustWrite(t, active, []byte{42}, 3*blockSize)
	if got := active.Stats().Evictions; got != 0 {
		t.Fatalf("evicted %d active blocks despite available reuse", got)
	}
	for i := range 4 {
		var got [1]byte
		if _, err := active.ReadAt(got[:], int64(i*blockSize)); err != nil || got[0] != 42 {
			t.Fatalf("active block %d was removed: data=%v err=%v", i, got, err)
		}
	}
	if stats := p.Stats(); stats.MemoryInUse != 4*blockSize || stats.MemoryAllocated != 4*blockSize {
		t.Fatalf("allocation limit changed during admission: %+v", stats)
	}
}

func TestPendingBlockRemainsChargedUntilRelease(t *testing.T) {
	t.Parallel()
	p := newTestPool(t, PoolConfig{})
	a := blockAllocator{pool: p}
	data := a.get()
	// Hold a release request before the worker processes it.
	pending := make(chan blockRelease, 1)
	pending <- blockRelease{data: data, pool: p}
	if got := p.Stats().MemoryAllocated; got != blockSize {
		t.Fatalf("pending allocation = %d, want %d", got, blockSize)
	}
	(<-pending).release()
	if got := p.Stats().MemoryAllocated; got != 0 {
		t.Fatalf("released allocation remains charged: %d", got)
	}
}

func TestCloseReleasesRetainedAndDeferredAllocations(t *testing.T) {
	t.Parallel()
	p := newTestPool(t, PoolConfig{})
	b := newTestBuffer(t, p, Config{MemorySize: 16 * blockSize})
	data := make([]byte, 16*blockSize)
	mustWrite(t, b, data, 0)
	mustDiscard(t, b, 0, int64(len(data)))
	if stats := p.Stats(); stats.MemoryInUse != 0 || stats.MemoryAllocated < maxReuseBlocks*blockSize {
		t.Fatalf("reuse or deferred blocks are not charged: %+v", stats)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "all allocations released after close", func() bool {
		return p.Stats().MemoryAllocated == 0
	})
}

// Each Pool owns its unmap worker: Close drains it and stops the goroutine,
// and blocks released afterwards are unmapped inline, not leaked.
func TestPoolCloseDrainsAndStopsUnmapper(t *testing.T) {
	t.Parallel()
	p := NewPool(PoolConfig{})
	a := &blockAllocator{pool: p} // no reuse: every put is released
	for range 4 {
		a.put(a.get())
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if got := p.Stats().MemoryAllocated; got != 0 {
		t.Fatalf("closed pool still charges %d bytes", got)
	}
	p.unmap.mu.RLock()
	done := p.unmap.done
	p.unmap.mu.RUnlock()
	select {
	case <-done:
	default:
		t.Fatal("unmap worker still running after Close")
	}

	a.put(a.get())
	if got := p.Stats().MemoryAllocated; got != 0 {
		t.Fatalf("release after Close left %d bytes charged", got)
	}
}
