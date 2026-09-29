package share

import (
	"context"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/manager"
)

type emptyCatalog struct{}

func (emptyCatalog) RootInfo() *manager.FileInfo   { return &manager.FileInfo{} }
func (emptyCatalog) GetEntries() []manager.FileInfo { return nil }
func (emptyCatalog) GetEntryChildren(string) (*manager.FileInfo, []manager.FileInfo) {
	return nil, nil
}

func (emptyCatalog) GetTorrentChildren(string) (*manager.FileInfo, []manager.FileInfo) {
	return nil, nil
}

// Expired lookups must be purged even when their path is never asked for
// again; otherwise every name a client ever looked up (misses included)
// stays in memory forever.
func TestLookupMemoPurgesExpiredEntries(t *testing.T) {
	t.Parallel()
	f := newFilesystem(emptyCatalog{}, nil)
	f.lookups.Store("stale", &lookupCacheEntry{expiry: 1})

	if _, err := f.Stat(context.Background(), "/missing"); err == nil {
		t.Fatal("Stat of a missing path succeeded")
	}
	if _, ok := f.lookups.Load("stale"); ok {
		t.Fatal("expired lookup survived a sweep")
	}
	if _, ok := f.lookups.Load("missing"); !ok {
		t.Fatal("fresh lookup was not memoized")
	}
}
