package manager

import (
	"strings"
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// GrabView is a reader's copy of one grab: its state and its queue entry.
type GrabView struct {
	State grab.State
	Entry storage.Entry
}

func (v GrabView) clone() GrabView {
	v.Entry = *v.Entry.Clone()
	return v
}

// grabIndex is the in-memory read index of grabs, written only by their entry
// actors. Every view goes in and comes out as a copy; no callback runs under mu.
type grabIndex struct {
	mu    sync.RWMutex
	views map[string]GrabView
	gone  map[string][]chan struct{}
	// seen is the failure time an Arr listing last rendered for each failed grab. It is not persisted, and it stays
	// while the grab's actor is idle.
	seen map[string]time.Time
	// polled is what the last poll of each grab saw at its provider; like seen, it is not persisted and stays while the
	// actor is idle, so a deadline derived from it still fires.
	polled map[string]pollSeen
	// submits is the clock of each submitting grab; like polled, it stays while the actor is idle, so the lookup
	// deadline and backoff do not restart each time a timer wakes the actor.
	submits map[string]submitClock
	// nudges counts the selects and restarts sent for each grab's placement in this phase; like polled, it stays while
	// the actor is idle, so the cap holds across polls spaced further apart than the idle limit.
	nudges map[string]int
}

// submitClock is when this process loaded a submitting grab (zero if it entered Submitting here), when its submit
// child last ended here, and how many lookups have run since.
type submitClock struct {
	loaded, exited time.Time
	lookups        int
}

func (x *grabIndex) publish(v GrabView) {
	v = v.clone()
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.views == nil {
		x.views = make(map[string]GrabView)
	}
	x.views[strings.ToLower(v.State.Key)] = v
}

func (x *grabIndex) remove(key string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	key = strings.ToLower(key)
	delete(x.views, key)
	for _, ch := range x.gone[key] {
		close(ch)
	}
	delete(x.gone, key)
	delete(x.seen, key)
	delete(x.polled, key)
	delete(x.submits, key)
	delete(x.nudges, key)
}

// goneChan is closed when key has no grab.
func (x *grabIndex) goneChan(key string) <-chan struct{} {
	key = strings.ToLower(key)
	x.mu.Lock()
	defer x.mu.Unlock()
	ch := make(chan struct{})
	if _, ok := x.views[key]; !ok {
		close(ch)
		return ch
	}
	if x.gone == nil {
		x.gone = make(map[string][]chan struct{})
	}
	x.gone[key] = append(x.gone[key], ch)
	return ch
}

func (x *grabIndex) get(key string) (GrabView, bool) {
	x.mu.RLock()
	v, ok := x.views[strings.ToLower(key)]
	x.mu.RUnlock()
	if !ok {
		return GrabView{}, false
	}
	return v.clone(), true
}

// active reports whether key has a grab that is still in progress.
func (x *grabIndex) active(key string) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	v, ok := x.views[strings.ToLower(key)]
	if !ok {
		return false
	}
	switch v.State.Phase {
	case grab.PhaseAbsent, grab.PhaseComplete, grab.PhaseFailed:
		return false
	case grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseProvider,
		grab.PhaseAwaitingLinks, grab.PhaseActing, grab.PhaseDeleting:
		return true
	}
	return false
}

func (x *grabIndex) snapshot() []GrabView {
	x.mu.RLock()
	out := make([]GrabView, 0, len(x.views))
	for _, v := range x.views {
		out = append(out, v)
	}
	x.mu.RUnlock()
	for i := range out {
		out[i] = out[i].clone()
	}
	return out
}

// markSeen records that an Arr listing rendered key's failure at at, to the second.
func (x *grabIndex) markSeen(key string, at time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.seen == nil {
		x.seen = make(map[string]time.Time)
	}
	x.seen[strings.ToLower(key)] = at.Truncate(time.Second)
}

// seenFailure is the failure time an Arr listing last rendered for key, or zero.
func (x *grabIndex) seenFailure(key string) time.Time {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.seen[strings.ToLower(key)]
}

// setPolled records what the last poll of key's placement saw.
func (x *grabIndex) setPolled(key string, p pollSeen) {
	x.mu.Lock()
	defer x.mu.Unlock()
	key = strings.ToLower(key)
	if p == (pollSeen{}) {
		delete(x.polled, key)
		return
	}
	if x.polled == nil {
		x.polled = make(map[string]pollSeen)
	}
	x.polled[key] = p
}

// polledOf is what the last poll of key's placement saw, or the zero value.
func (x *grabIndex) polledOf(key string) pollSeen {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.polled[strings.ToLower(key)]
}

// setSubmitClock records key's submit clock; the zero clock forgets it.
func (x *grabIndex) setSubmitClock(key string, c submitClock) {
	x.mu.Lock()
	defer x.mu.Unlock()
	key = strings.ToLower(key)
	if c == (submitClock{}) {
		delete(x.submits, key)
		return
	}
	if x.submits == nil {
		x.submits = make(map[string]submitClock)
	}
	x.submits[key] = c
}

// submitClockOf is key's submit clock, or the zero value.
func (x *grabIndex) submitClockOf(key string) submitClock {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.submits[strings.ToLower(key)]
}

// nudgesOf is how many selects and restarts key's placement has been sent in this phase.
func (x *grabIndex) nudgesOf(key string) int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.nudges[strings.ToLower(key)]
}

// setNudges records how many selects and restarts key's placement has been sent; zero forgets the count.
func (x *grabIndex) setNudges(key string, n int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	key = strings.ToLower(key)
	if n == 0 {
		delete(x.nudges, key)
		return
	}
	if x.nudges == nil {
		x.nudges = make(map[string]int)
	}
	x.nudges[key] = n
}
