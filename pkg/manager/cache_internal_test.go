package manager

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// A Get issued after InvalidateAll never receives a listing loaded before it,
// whether by joining that load or by reading what the load stored.
func TestEntryCacheGetAfterInvalidateAllIsFresh(t *testing.T) {
	t.Parallel()
	synctest.Test(t, testEntryCacheGetAfterInvalidateAllIsFresh)
}

func testEntryCacheGetAfterInvalidateAllIsFresh(t *testing.T) {
	var loads atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	load := func(name string) (*FileInfo, []FileInfo) {
		version := loads.Add(1)
		if version == 1 {
			close(started)
			<-release
		}
		return &FileInfo{name: name, size: version}, nil
	}
	cache := newEntryCache(func(string) bool { return false }, load, load)

	first := make(chan *FileInfo, 1)
	go func() {
		current, _ := cache.Get("group")
		first <- current
	}()
	<-started
	cache.InvalidateAll()

	second := make(chan *FileInfo, 1)
	go func() {
		current, _ := cache.Get("group")
		second <- current
	}()
	// The post-invalidation Get must not wait on the stale load.
	synctest.Wait()
	select {
	case got := <-second:
		if got.size != 2 {
			t.Errorf("Get after InvalidateAll returned load %d, want a fresh load", got.size)
		}
	default:
		t.Error("Get after InvalidateAll waited on a load that started before it")
	}
	close(release)
	if t.Failed() {
		<-second
		return
	}
	if got := <-first; got.size != 1 {
		t.Fatalf("first Get returned load %d, want its own load", got.size)
	}
	if current, _ := cache.Get("group"); current.size != 2 {
		t.Fatalf("cache serves load %d after the stale load finished, want 2", current.size)
	}
}

// A load that started before InvalidateAll and finishes after it is not
// served to later Gets, even when it is the only load stored.
func TestEntryCacheStaleLoadIsNotServed(t *testing.T) {
	t.Parallel()
	var loads atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	load := func(name string) (*FileInfo, []FileInfo) {
		version := loads.Add(1)
		if version == 1 {
			close(started)
			<-release
		}
		return &FileInfo{name: name, size: version}, nil
	}
	cache := newEntryCache(func(string) bool { return false }, load, load)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.Get(torrentEntryCachePrefix + "movie")
	}()
	<-started
	cache.InvalidateAll()
	close(release)
	<-done

	if current, _ := cache.Get(torrentEntryCachePrefix + "movie"); current.size != 2 {
		t.Fatalf("Get after InvalidateAll served load %d, want a fresh load", current.size)
	}
}
