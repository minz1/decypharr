package storage

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Every write marks the store's cached key order dirty, and appendstore
// rebuilds it inside ForEach under a read lock. Scans started together after
// a write must not race on that rebuild (run with -race).
func TestConcurrentScansAfterWritesDoNotRace(t *testing.T) {
	t.Parallel()
	s, err := NewStorage(filepath.Join(t.TempDir(), "db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for round := range 20 {
		entry := &Entry{InfoHash: fmt.Sprintf("%040d", round), Name: fmt.Sprintf("entry-%d", round)}
		if addErr := s.AddOrUpdate(entry); addErr != nil {
			t.Fatal(addErr)
		}
		var wg sync.WaitGroup
		seen := make([]int, 4)
		for i := range seen {
			wg.Go(func() {
				_ = s.ForEach(func(*Entry) error {
					seen[i]++
					return nil
				})
			})
		}
		wg.Wait()
		for i, n := range seen {
			if n != round+1 {
				t.Fatalf("round %d: scan %d saw %d entries, want %d", round, i, n, round+1)
			}
		}
	}
}

// A scan callback may start another scan of the same store.
func TestNestedScanDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	s, err := NewStorage(filepath.Join(t.TempDir(), "db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if addErr := s.AddOrUpdate(&Entry{InfoHash: fmt.Sprintf("%040d", 1), Name: "entry"}); addErr != nil {
		t.Fatal(addErr)
	}
	inner := 0
	if scanErr := s.ForEach(func(*Entry) error {
		return s.ForEach(func(*Entry) error {
			inner++
			return nil
		})
	}); scanErr != nil {
		t.Fatal(scanErr)
	}
	if inner != 1 {
		t.Fatalf("nested scan saw %d entries, want 1", inner)
	}
}
