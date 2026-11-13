package manager

import (
	"strings"
	"sync"
	"time"
)

// reinsertCooldown is how long a provider that refused a re-insert is skipped for the same entry.
const reinsertCooldown = 6 * time.Hour

type keyRepair struct {
	run         *FixerRequest
	failedUntil map[string]time.Time
}

// repairTable holds, per entry key, the repair that is running and the providers that refused a re-insert until
// their cooldown ends. A key with neither is not kept. Its methods take only mu: they never lock a stripe or call
// storage, so a caller may hold a stripe, but begin is called outside any stripe.
type repairTable struct {
	mu   sync.Mutex
	keys map[string]*keyRepair
}

func (t *repairTable) entry(key string) *keyRepair {
	if t.keys == nil {
		t.keys = make(map[string]*keyRepair)
	}
	k := t.keys[key]
	if k == nil {
		k = &keyRepair{}
		t.keys[key] = k
	}
	return k
}

func (t *repairTable) prune(key string, k *keyRepair) {
	if k.run == nil && len(k.failedUntil) == 0 {
		delete(t.keys, key)
	}
}

// begin registers req as key's repair and reports true, or returns the repair already running and false.
func (t *repairTable) begin(key string, req *FixerRequest) (*FixerRequest, bool) {
	key = strings.ToLower(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.entry(key)
	if k.run != nil {
		return k.run, false
	}
	k.run = req
	return req, true
}

// end clears key's running repair.
func (t *repairTable) end(key string) {
	key = strings.ToLower(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	if k := t.keys[key]; k != nil {
		k.run = nil
		t.prune(key, k)
	}
}

// running reports whether a repair of key is running.
func (t *repairTable) running(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.keys[strings.ToLower(key)]
	return k != nil && k.run != nil
}

// failed reports whether provider refused a re-insert of key and its cooldown has not ended at now.
func (t *repairTable) failed(key, provider string, now time.Time) bool {
	key = strings.ToLower(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.keys[key]
	if k == nil {
		return false
	}
	until, ok := k.failedUntil[provider]
	if ok && now.Before(until) {
		return true
	}
	delete(k.failedUntil, provider)
	t.prune(key, k)
	return false
}

// fail records that provider refused a re-insert of key; it is skipped until the cooldown ends.
func (t *repairTable) fail(key, provider string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.entry(strings.ToLower(key))
	if k.failedUntil == nil {
		k.failedUntil = make(map[string]time.Time)
	}
	k.failedUntil[provider] = now.Add(reinsertCooldown)
}

// reset forgets every failure recorded for key; a running repair stays.
func (t *repairTable) reset(key string) {
	key = strings.ToLower(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	if k := t.keys[key]; k != nil {
		k.failedUntil = nil
		t.prune(key, k)
	}
}

func (t *repairTable) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.keys)
}
