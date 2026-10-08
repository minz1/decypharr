package buffer

import "sync"

// unmapper is a Pool's background unmap worker. munmap is a TLB-shootdown
// syscall; on Linux it can stall for tens of microseconds and, worse, the
// eviction path calls put() while the owning Buffer holds its exclusive lock
// (dropBlockLocked runs under b.mu). Running the syscall there stalls every
// waiting reader — the tail-latency regression vs the old [sync.Pool], whose
// Put never syscalls. Handing overflow blocks to the pool's goroutine keeps
// the lock holder off the syscall: the pages are still released promptly
// (the queue drains continuously), only the *timing* moves off the critical
// path, so deterministic RAM release is preserved.
//
// The goroutine starts with the first release and stops at Pool.Close, after
// draining what is queued; later releases run inline. The zero value is
// ready to use.
type unmapper struct {
	mu     sync.RWMutex
	ch     chan blockRelease
	closed bool
	done   chan struct{}
}

// unmapQueueDepth bounds pending background releases before put falls back to
// an inline unmap.
const unmapQueueDepth = 256

// release queues r for the worker, or releases it inline when the queue is
// full or the worker has stopped.
func (u *unmapper) release(r blockRelease) {
	u.mu.RLock()
	if u.ch == nil && !u.closed {
		u.mu.RUnlock()
		u.start()
		u.mu.RLock()
	}
	if !u.closed {
		select {
		case u.ch <- r:
			u.mu.RUnlock()
			return
		default:
		}
	}
	u.mu.RUnlock()
	r.release()
}

func (u *unmapper) start() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ch != nil || u.closed {
		return
	}
	u.ch = make(chan blockRelease, unmapQueueDepth)
	u.done = make(chan struct{})
	go func(ch <-chan blockRelease, done chan<- struct{}) {
		defer close(done)
		for r := range ch {
			r.release()
		}
	}(u.ch, u.done)
}

// close drains the queue and stops the worker.
func (u *unmapper) close() {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.closed = true
	ch, done := u.ch, u.done
	if ch != nil {
		close(ch)
	}
	u.mu.Unlock()
	if done != nil {
		<-done
	}
}

// blockRelease keeps an allocation charged until its release completes.
type blockRelease struct {
	data *[]byte
	pool *Pool
}

func (r blockRelease) release() {
	munmapBlock(r.data)
	r.pool.memAllocated.Add(-blockSize)
}

// releaseBlock unmaps p off the caller's goroutine when possible, falling back
// to an inline unmap only if the queue is saturated (rare churn burst) or the
// pool is closed.
func releaseBlock(p *[]byte, pool *Pool) {
	if p == nil {
		return
	}
	pool.unmap.release(blockRelease{data: p, pool: pool})
}

// maxReuseBlocks caps how many freed blocks a blockAllocator keeps on hand
// for reuse. The free list exists only to absorb steady-state churn —
// where every eviction is immediately followed by a load of the next block
// — so it can stay small. Anything freed beyond this is unmapped at once,
// which is what makes a shrinking working set actually return RAM. 8 blocks
// = 8 MB of reuse headroom per Buffer, more than enough for sequential
// streaming's evict-then-load cadence.
const maxReuseBlocks = 8

// blockAllocator hands out fixed blockSize buffers and returns them to the
// OS when they are no longer needed.
//
// It replaces the per-Buffer [sync.Pool]. A [sync.Pool] only releases its
// contents lazily — entries survive until a GC, and the freed heap pages
// return to the OS later still, on the scavenger's schedule (MADV_FREE).
// Under many concurrent streams that lag let RSS climb into OOM territory.
// This allocator instead frees via munmapBlock the moment a block falls out
// of the small reuse window, so memory tracks the live working set closely
// without relying on GC pacing or GOMEMLIMIT.
//
// get/put are currently only called with the owning Buffer's mu held, but the
// allocator carries its own mutex so that contract isn't load-bearing. Lock
// order is always Buffer.mu -> blockAllocator.mu; the allocator never reaches
// back for Buffer.mu, so this can't deadlock.
type blockAllocator struct {
	mu      sync.Mutex
	free    []*[]byte
	maxFree int
	pool    *Pool
}

// get returns a blockSize buffer, reusing a freed one when available.
func (a *blockAllocator) get() *[]byte {
	a.mu.Lock()
	if n := len(a.free); n > 0 {
		p := a.free[n-1]
		a.free[n-1] = nil
		a.free = a.free[:n-1]
		a.pool.memReusable.Add(-blockSize)
		a.mu.Unlock()
		return p
	}
	a.mu.Unlock()
	a.pool.memAllocated.Add(blockSize)
	return mmapAlloc(blockSize)
}

// put retains a block if the free list and pool budget have room.
// Otherwise, it sends the block for release.
func (a *blockAllocator) put(p *[]byte) {
	if p == nil {
		return
	}
	a.mu.Lock()
	if len(a.free) < a.maxFree && !a.pool.overMemBudget(a.pool.memAllocated.Load()) {
		a.free = append(a.free, p)
		a.pool.memReusable.Add(blockSize)
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	// Off the caller's goroutine: put() is called under the Buffer's exclusive
	// lock on the eviction path, and munmap must not stall readers there.
	releaseBlock(p, a.pool)
}

// drain releases all reuse blocks on close or under pool pressure.
func (a *blockAllocator) drain() {
	a.mu.Lock()
	free := a.free
	a.free = nil
	a.pool.memReusable.Add(-int64(len(free)) * blockSize)
	a.mu.Unlock()
	for _, p := range free {
		blockRelease{data: p, pool: a.pool}.release()
	}
}
