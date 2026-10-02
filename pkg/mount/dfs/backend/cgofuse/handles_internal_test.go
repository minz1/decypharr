package cgofuse

import (
	"sync"
	"testing"
)

// Concurrent opens must never share a handle ID: a duplicate overwrote the
// first handle in the map, leaking its reader (and the cache item's open
// reference) forever.
func TestHandleManagerCreateUniqueUnderConcurrency(t *testing.T) {
	t.Parallel()
	const n = 2000
	h := NewHandleManager()
	ids := make([]uint64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { ids[i] = h.Create(nil, nil) })
	}
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("handle ID %d issued twice", id)
		}
		seen[id] = true
	}
	if got := h.handles.Size(); got != n {
		t.Fatalf("stored handles = %d, want %d", got, n)
	}
}

// A handle must be released exactly once even when Release races CloseAll.
func TestHandleManagerTakeIsExactlyOnce(t *testing.T) {
	t.Parallel()
	h := NewHandleManager()
	fh := h.Create(nil, nil)

	var mu sync.Mutex
	released := 0
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, ok := h.Take(fh); ok {
			mu.Lock()
			released++
			mu.Unlock()
		}
	})
	wg.Go(func() {
		h.CloseAll(func(*FileHandle) {
			mu.Lock()
			released++
			mu.Unlock()
		})
	})
	wg.Wait()
	if released != 1 {
		t.Fatalf("handle released %d times, want 1", released)
	}
}
