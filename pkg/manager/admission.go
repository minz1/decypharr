package manager

import (
	"slices"
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

// admission grants download slots to grabs, FIFO, up to capacity (one global cap;
// per-provider slots come with placements). It has no mailbox: every send to an
// entry actor happens after mu is released.
type admission struct {
	mu        sync.Mutex
	capacity  int
	holding   map[string]bool
	waiting   []string
	throttled map[string][]string // provider -> keys throttled on it
	send      func(key string, ev grab.Event) error
}

func newAdmission(capacity int, send func(key string, ev grab.Event) error) *admission {
	return &admission{
		capacity:  capacity,
		holding:   map[string]bool{},
		throttled: map[string][]string{},
		send:      send,
	}
}

// Request queues key for a slot (once per key) and grants what is free.
func (a *admission) Request(key string) {
	a.mu.Lock()
	if !a.holding[key] && !slices.Contains(a.waiting, key) {
		a.waiting = append(a.waiting, key)
	}
	grants := a.grantLocked()
	a.mu.Unlock()
	a.deliver(grants)
}

// Release frees key's slot, if it holds one, and grants the next.
func (a *admission) Release(key string) {
	a.mu.Lock()
	if !a.holding[key] {
		a.mu.Unlock()
		return
	}
	delete(a.holding, key)
	grants := a.grantLocked()
	a.mu.Unlock()
	a.deliver(grants)
}

// Cancel drops a waiting key.
func (a *admission) Cancel(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.waiting = slices.DeleteFunc(a.waiting, func(k string) bool { return k == key })
}

// Throttled records that key waits for a slot at any of providers.
func (a *admission) Throttled(key string, providers []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range providers {
		if !slices.Contains(a.throttled[p], key) {
			a.throttled[p] = append(a.throttled[p], key)
		}
	}
}

// SlotFreed wakes the keys throttled on provider, once.
func (a *admission) SlotFreed(provider string) {
	a.mu.Lock()
	keys := a.throttled[provider]
	delete(a.throttled, provider)
	a.mu.Unlock()
	for _, k := range keys {
		// A refused wake is covered by the key's own throttle timer.
		_ = a.send(k, grab.Event{Kind: grab.EvSlotFreed, At: time.Now()})
	}
}

func (a *admission) grantLocked() []string {
	var grants []string
	for len(a.holding) < a.capacity && len(a.waiting) > 0 {
		k := a.waiting[0]
		a.waiting = a.waiting[1:]
		a.holding[k] = true
		grants = append(grants, k)
	}
	return grants
}

// deliver sends EvAdmitted to each granted key. A refused grant returns its
// slot and puts the key back at the head of the line; the key's admission
// deadline timer requests it again.
func (a *admission) deliver(grants []string) {
	var refused []string
	for _, k := range grants {
		if err := a.send(k, grab.Event{Kind: grab.EvAdmitted, At: time.Now()}); err != nil {
			refused = append(refused, k)
		}
	}
	if len(refused) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, k := range refused {
		delete(a.holding, k)
	}
	a.waiting = append(refused, a.waiting...)
}
