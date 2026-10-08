package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/kvstore"

	"github.com/sirrobot01/appendstore"
)

// newQueueTestStorage returns a Storage backed only by a fresh queue store.
func newQueueTestStorage(t *testing.T) (*Storage, *kvstore.Store) {
	t.Helper()
	queue, err := kvstore.Open(filepath.Join(t.TempDir(), "queue.db"), appendstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	return &Storage{queue: queue}, queue
}

func TestQueueOperationsReportCorruptRecords(t *testing.T) {
	t.Parallel()
	operations := map[string]func(s *Storage, called *bool) error{
		"filter": func(s *Storage, _ *bool) error {
			_, err := s.FilterQueued(nil)
			return err
		},
		"delete": func(s *Storage, called *bool) error {
			return s.DeleteWhereQueued(nil, func(*Entry) error { *called = true; return nil })
		},
		"update": func(s *Storage, called *bool) error {
			return s.UpdateWhereQueued(
				nil,
				func(entry *Entry) bool { *called = true; entry.Name = "after"; return true },
			)
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, queue := newQueueTestStorage(t)
			if addQueueErr := s.AddQueue(&Entry{InfoHash: "valid", Name: "before"}); addQueueErr != nil {
				t.Fatal(addQueueErr)
			}
			if putErr := queue.Put("corrupt", []byte{0xff}, nil); putErr != nil {
				t.Fatal(putErr)
			}
			called := false
			if err := operation(s, &called); err == nil || !strings.Contains(err.Error(), "corrupt") {
				t.Fatalf("error = %v, want the corrupt record key", err)
			}
			if called {
				t.Fatal("mutation ran before the queue scan completed")
			}
			entry, err := s.GetQueued("valid")
			if err != nil || entry.Name != "before" {
				t.Fatalf("valid entry changed: entry=%v, err=%v", entry, err)
			}
		})
	}
}

func TestQueueOperationsReportClosedStore(t *testing.T) {
	t.Parallel()
	// closeFirst operations run against an already closed store; the others
	// close it during their write.
	operations := map[string]struct {
		closeFirst bool
		run        func(t *testing.T, s *Storage, queue *kvstore.Store) error
	}{
		"filter": {closeFirst: true, run: func(_ *testing.T, s *Storage, _ *kvstore.Store) error {
			_, err := s.FilterQueued(nil)
			return err
		}},
		"delete": {closeFirst: true, run: func(_ *testing.T, s *Storage, _ *kvstore.Store) error {
			return s.DeleteWhereQueued(nil, nil)
		}},
		"update": {closeFirst: true, run: func(_ *testing.T, s *Storage, _ *kvstore.Store) error {
			return s.UpdateWhereQueued(nil, func(*Entry) bool { return true })
		}},
		"delete write": {run: func(_ *testing.T, s *Storage, queue *kvstore.Store) error {
			return s.DeleteWhereQueued(nil, func(*Entry) error { return queue.Close() })
		}},
		"update write": {run: func(t *testing.T, s *Storage, queue *kvstore.Store) error {
			return s.UpdateWhereQueued(nil, func(*Entry) bool {
				if closeErr := queue.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				return true
			})
		}},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, queue := newQueueTestStorage(t)
			if addQueueErr := s.AddQueue(&Entry{InfoHash: "entry"}); addQueueErr != nil {
				t.Fatal(addQueueErr)
			}
			if operation.closeFirst {
				if closeErr := queue.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if err := operation.run(t, s, queue); !errors.Is(err, appendstore.ErrStoreClosed) {
				t.Fatalf("error = %v, want ErrStoreClosed", err)
			}
		})
	}
}

func TestQueueCleanupFailureRetainsEntry(t *testing.T) {
	t.Parallel()
	s, _ := newQueueTestStorage(t)
	for _, hash := range []string{"keep", "delete"} {
		if addQueueErr := s.AddQueue(&Entry{InfoHash: hash}); addQueueErr != nil {
			t.Fatal(addQueueErr)
		}
	}
	cleanupErr := errors.New("cleanup failed")
	err := s.DeleteWhereQueued(nil, func(entry *Entry) error {
		if entry.InfoHash == "keep" {
			return cleanupErr
		}
		return nil
	})
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want the cleanup error", err)
	}
	if _, getQueuedErr := s.GetQueued("keep"); getQueuedErr != nil {
		t.Fatalf("failed entry was removed: %v", getQueuedErr)
	}
	if _, getQueuedErr := s.GetQueued("delete"); !errors.Is(getQueuedErr, appendstore.ErrKeyNotFound) {
		t.Fatalf("successful entry remains: %v", getQueuedErr)
	}
}
