package types

import (
	"errors"
	"testing"
)

// SM12-16, accepted: Get holds its lock across the fetch, so a caller that arrives during a fetch (a stats refresh
// during the startup profile fetch) waits for that fetch and gets its result, never a second request.
func TestProfileCacheCallerDuringFetchWaitsForItsResult(t *testing.T) {
	t.Parallel()
	var cache ProfileCache
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := cache.Get(0, func() (*Profile, error) {
			close(entered)
			<-release
			return &Profile{Name: "startup"}, nil
		})
		first <- err
	}()
	<-entered
	if cache.mu.TryLock() {
		cache.mu.Unlock()
		close(release)
		t.Fatal("Get released its lock during the fetch: a caller arriving now would fetch again")
	}
	second := make(chan *Profile, 1)
	go func() {
		profile, err := cache.Get(0, func() (*Profile, error) {
			return nil, errors.New("a second fetch while one was in flight")
		})
		if err != nil {
			t.Error(err)
		}
		second <- profile
	}()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if profile := <-second; profile == nil || profile.Name != "startup" {
		t.Fatalf("the waiting caller got %v, want the in-flight fetch's profile", profile)
	}
}
