package types_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// Run with -race: callers mutate their result while others read the cache.
func TestProfileCacheIsConcurrentAndCopies(t *testing.T) {
	t.Parallel()
	var cache types.ProfileCache
	var fetches atomic.Int32
	fetch := func() (*types.Profile, error) {
		fetches.Add(1)
		return &types.Profile{Name: "provider"}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			profile, err := cache.Get(0, fetch)
			if err != nil {
				t.Error(err)
				return
			}
			profile.Name = "changed by caller"
		})
	}
	wg.Wait()
	profile, err := cache.Get(0, fetch)
	if err != nil || profile.Name != "provider" || fetches.Load() != 1 {
		t.Fatalf("profile=%v err=%v fetches=%d, want one fetch and an unmodified cache", profile, err, fetches.Load())
	}
}

func TestProfileCacheRefreshesAfterTTLAndKeepsErrorsUncached(t *testing.T) {
	t.Parallel()
	var cache types.ProfileCache
	failure := errors.New("unavailable")
	if _, err := cache.Get(time.Hour, func() (*types.Profile, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatalf("Get() error = %v, want fetch failure", err)
	}
	if _, err := cache.Get(time.Nanosecond, func() (*types.Profile, error) { return &types.Profile{Id: 1}, nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	profile, err := cache.Get(time.Nanosecond, func() (*types.Profile, error) { return &types.Profile{Id: 2}, nil })
	if err != nil || profile.Id != 2 {
		t.Fatalf("profile=%v err=%v, want refreshed profile", profile, err)
	}
}
