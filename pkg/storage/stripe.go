package storage

import (
	"errors"
	"hash/fnv"
	"slices"
	"strings"

	"github.com/sirrobot01/appendstore"
)

// entryStripes is the number of library key locks.
const entryStripes = 64

func stripeOf(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(key)))
	return int(h.Sum32() % entryStripes)
}

// LockEntry takes the lock of key's stripe; every read-modify-write of a library
// entry holds it. Keys are case-insensitive.
func (s *Storage) LockEntry(key string) func() {
	mu := &s.stripes[stripeOf(key)]
	mu.Lock()
	return mu.Unlock
}

// LockEntries takes the stripes of every key, each once, in ascending order.
func (s *Storage) LockEntries(keys []string) func() {
	idx := make([]int, 0, len(keys))
	for _, k := range keys {
		idx = append(idx, stripeOf(k))
	}
	slices.Sort(idx)
	idx = slices.Compact(idx)
	for _, i := range idx {
		s.stripes[i].Lock()
	}
	return func() {
		for _, i := range slices.Backward(idx) {
			s.stripes[i].Unlock()
		}
	}
}

// ErrNoChange is returned by a MergeEntry callback to write nothing; MergeEntry then returns nil.
var ErrNoChange = errors.New("storage: no change")

// MergeEntry reads key's entry (nil when missing) under its stripe, passes it to
// merge and writes the result. An error writes nothing; ErrNoChange is not
// returned. The caller must not hold the stripe, and merge must not lock.
func (s *Storage) MergeEntry(key string, merge func(fresh *Entry) (*Entry, error)) error {
	defer s.LockEntry(key)()
	return s.MergeEntryLocked(key, merge)
}

// MergeEntryLocked is MergeEntry for a caller that already holds key's stripe.
func (s *Storage) MergeEntryLocked(key string, merge func(fresh *Entry) (*Entry, error)) error {
	fresh, err := s.Get(key)
	if err != nil && !errors.Is(err, appendstore.ErrKeyNotFound) {
		return err
	}
	out, err := merge(fresh)
	if errors.Is(err, ErrNoChange) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.AddOrUpdate(out)
}

// PublishEntry writes incoming merged into the stored entry for its hash: placements,
// files and tags the store knows survive, and Bad is never cleared.
func (s *Storage) PublishEntry(incoming *Entry) error {
	return s.MergeEntry(incoming.InfoHash, func(fresh *Entry) (*Entry, error) {
		if fresh == nil {
			return incoming, nil
		}
		return HandleExistingEntryMerge(fresh, incoming), nil
	})
}
