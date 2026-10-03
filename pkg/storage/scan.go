package storage

import "github.com/sirrobot01/appendstore"

// forEach is store.ForEach made safe for concurrent scans.
//
// appendstore v0.6.0 rebuilds its cached key order inside ForEach while
// holding only the store's read lock, so two scans of one store started
// together race on that cache (a reader can see a half-written key list).
// ForEach takes its key snapshot before it calls fn the first time, so
// scanMu is held from the start of the scan until the first callback (or
// the end of an empty scan) and no longer: callbacks never run under it,
// and a callback that starts another scan cannot deadlock.
func (s *Storage) forEach(store *appendstore.Store, fn func(key string, value []byte) error) error {
	s.scanMu.Lock()
	locked := true
	release := func() {
		if locked {
			locked = false
			s.scanMu.Unlock()
		}
	}
	defer release()
	return store.ForEach(func(key string, value []byte) error {
		release()
		return fn(key, value)
	})
}
