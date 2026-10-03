// Package kvstore wraps appendstore so every caller gets the same
// concurrency workaround.
package kvstore

import (
	"sync"

	"github.com/sirrobot01/appendstore"
)

// Store is an appendstore.Store whose ForEach is safe for concurrent scans.
//
// appendstore v0.6.0 rebuilds its cached key order inside ForEach while
// holding only the store's read lock, so two scans started together race on
// that cache (a reader can see a half-written key list). The real fix
// belongs upstream in appendstore; until then every scan goes through here.
type Store struct {
	*appendstore.Store

	scanMu sync.Mutex
}

// Open opens the store at path.
func Open(path string, options appendstore.Options) (*Store, error) {
	store, err := appendstore.Open(path, options)
	if err != nil {
		return nil, err
	}
	return &Store{Store: store}, nil
}

// ForEach calls fn for every key and value, like appendstore's ForEach.
// ForEach takes its key snapshot before it calls fn the first time, so the
// gate is held from the start of the scan until the first callback (or the
// end of an empty scan) and no longer: callbacks never run under it, and a
// callback that starts another scan cannot deadlock.
func (s *Store) ForEach(fn func(key string, value []byte) error) error {
	s.scanMu.Lock()
	locked := true
	release := func() {
		if locked {
			locked = false
			s.scanMu.Unlock()
		}
	}
	defer release()
	return s.Store.ForEach(func(key string, value []byte) error {
		release()
		return fn(key, value)
	})
}
