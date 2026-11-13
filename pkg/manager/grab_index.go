package manager

import (
	"strings"
	"sync"

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
	delete(x.views, strings.ToLower(key))
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
