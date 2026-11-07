package repair

import (
	"sync"
)

// probeTracker counts the canary probes in flight so Stop can wait for them. A WaitGroup would do, but probes
// start at any time, including while Stop waits, which a WaitGroup does not allow from a count of zero.
type probeTracker struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

// begin registers a probe and returns the function that ends it.
func (t *probeTracker) begin() func() {
	t.mu.Lock()
	t.n++
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.n--
		if t.n == 0 && t.idle != nil {
			close(t.idle)
			t.idle = nil
		}
	}
}

// wait returns once no probe is in flight, or when expired is closed, and reports whether it ended idle.
func (t *probeTracker) wait(expired <-chan struct{}) bool {
	t.mu.Lock()
	if t.n == 0 {
		t.mu.Unlock()
		return true
	}
	if t.idle == nil {
		t.idle = make(chan struct{})
	}
	idle := t.idle
	t.mu.Unlock()
	select {
	case <-idle:
		return true
	case <-expired:
		return false
	}
}
