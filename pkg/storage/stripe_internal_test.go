package storage

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestLibraryStripeSerialisesSyncAndPublish(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	const key = "abc"
	addProvider := func(name string, at time.Time) func(fresh *Entry) (*Entry, error) {
		return func(fresh *Entry) (*Entry, error) {
			if s.stripes[stripeOf(key)].TryLock() {
				s.stripes[stripeOf(key)].Unlock()
				t.Error("merge ran without the key's stripe held")
			}
			if fresh == nil {
				fresh = &Entry{InfoHash: key, Protocol: config.ProtocolTorrent}
			}
			if fresh.Providers == nil {
				fresh.Providers = map[string]*ProviderEntry{}
			}
			fresh.Providers[name] = &ProviderEntry{Provider: name, AddedAt: at}
			return fresh, nil
		}
	}
	now := time.Unix(1_700_000_000, 0)
	if err := s.MergeEntry(key, addProvider("a", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.MergeEntry(key, addProvider("b", now)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Providers["a"] == nil || got.Providers["b"] == nil {
		t.Fatalf("providers = %v, want a and b", got.Providers)
	}
}

func TestLockEntryIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	unlock := s.LockEntry("ABCDEF")
	if s.stripes[stripeOf("abcdef")].TryLock() {
		t.Fatal("the lower-cased key's stripe is free while the upper-cased key is locked")
	}
	unlock()
	if !s.stripes[stripeOf("abcdef")].TryLock() {
		t.Fatal("stripe still held after unlock")
	}
}

func TestLockEntriesSameStripeTwiceDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	first := "k0"
	var second string
	for i := 1; i < 10_000; i++ {
		k := fmt.Sprintf("k%d", i)
		if stripeOf(k) == stripeOf(first) {
			second = k
			break
		}
	}
	if second == "" {
		t.Fatal("no colliding key found")
	}
	unlock := s.LockEntries([]string{second, "other", first, first})
	if s.stripes[stripeOf(first)].TryLock() {
		t.Fatal("stripe free while locked")
	}
	unlock()
	for i := range s.stripes {
		if !s.stripes[i].TryLock() {
			t.Fatalf("stripe %d still held after unlock", i)
		}
	}
}

func TestMergeEntryNoChangeWritesNothing(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	var saw *Entry
	err := s.MergeEntry("missing", func(fresh *Entry) (*Entry, error) {
		saw = fresh
		return &Entry{InfoHash: "missing"}, ErrNoChange
	})
	if err != nil {
		t.Fatal(err)
	}
	if saw != nil {
		t.Fatalf("fresh = %v, want nil for a missing key", saw)
	}
	if _, getErr := s.Get("missing"); !errors.Is(getErr, appendstore.ErrKeyNotFound) {
		t.Fatalf("Get err = %v, want not found", getErr)
	}
}

func TestMergeEntryErrorWritesNothing(t *testing.T) {
	t.Parallel()
	s := openTestStorage(t, t.TempDir())
	defer s.Close()
	boom := errors.New("boom")
	err := s.MergeEntry("k", func(*Entry) (*Entry, error) {
		return &Entry{InfoHash: "k"}, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if _, getErr := s.Get("k"); !errors.Is(getErr, appendstore.ErrKeyNotFound) {
		t.Fatalf("Get err = %v, want not found", getErr)
	}
}

func TestPublishKeepsBad(t *testing.T) {
	t.Parallel()
	for _, protocol := range []config.Protocol{config.ProtocolTorrent, config.ProtocolNZB} {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			s := openTestStorage(t, t.TempDir())
			defer s.Close()
			if err := s.AddOrUpdate(&Entry{InfoHash: "h", Protocol: protocol, Bad: true}); err != nil {
				t.Fatal(err)
			}
			if err := s.PublishEntry(&Entry{InfoHash: "h", Protocol: protocol}); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get("h")
			if err != nil {
				t.Fatal(err)
			}
			if !got.Bad {
				t.Fatal("a publish cleared Bad")
			}
		})
	}
}
