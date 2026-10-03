package kvstore_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/kvstore"
)

// Two scans and a writer run together: every write marks appendstore's
// cached key order dirty and ForEach rebuilds it under a read lock, so
// unguarded scans race on the rebuild (run with -race).
func TestConcurrentScansWithWriterDoNotRace(t *testing.T) {
	t.Parallel()
	store, err := kvstore.Open(filepath.Join(t.TempDir(), "kv"), appendstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const writes = 200
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range writes {
			if putErr := store.Put(fmt.Sprintf("key-%03d", i), []byte("v"), nil); putErr != nil {
				t.Error(putErr)
				return
			}
		}
	})
	for range 2 {
		wg.Go(func() {
			for range writes / 4 {
				if scanErr := store.ForEach(func(string, []byte) error { return nil }); scanErr != nil {
					t.Error(scanErr)
					return
				}
			}
		})
	}
	wg.Wait()

	seen := 0
	if scanErr := store.ForEach(func(string, []byte) error { seen++; return nil }); scanErr != nil {
		t.Fatal(scanErr)
	}
	if seen != writes {
		t.Fatalf("scan saw %d keys, want %d", seen, writes)
	}
}

// A scan callback may start another scan of the same store.
func TestNestedScanDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	store, err := kvstore.Open(filepath.Join(t.TempDir(), "kv"), appendstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if putErr := store.Put("a", []byte("v"), nil); putErr != nil {
		t.Fatal(putErr)
	}
	inner := 0
	if scanErr := store.ForEach(func(string, []byte) error {
		return store.ForEach(func(string, []byte) error { inner++; return nil })
	}); scanErr != nil {
		t.Fatal(scanErr)
	}
	if inner != 1 {
		t.Fatalf("nested scan saw %d keys, want 1", inner)
	}
}
