package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirrobot01/appendstore"
)

func TestQueueOperationsReportCorruptRecords(t *testing.T) {
	for _, operation := range []string{"filter", "delete", "update"} {
		t.Run(operation, func(t *testing.T) {
			queue, err := appendstore.Open(filepath.Join(t.TempDir(), "queue.db"), appendstore.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = queue.Close() })
			s := &Storage{queue: queue}
			if addQueueErr := s.AddQueue(&Entry{InfoHash: "valid", Name: "before"}); addQueueErr != nil {
				t.Fatal(addQueueErr)
			}
			if putErr := queue.Put("corrupt", []byte{0xff}, nil); putErr != nil {
				t.Fatal(putErr)
			}
			called := false
			switch operation {
			case "filter":
				_, err = s.FilterQueued(nil)
			case "delete":
				err = s.DeleteWhereQueued(nil, func(*Entry) error { called = true; return nil })
			case "update":
				err = s.UpdateWhereQueued(
					nil,
					func(entry *Entry) bool { called = true; entry.Name = "after"; return true },
				)
			}
			if err == nil || !strings.Contains(err.Error(), "corrupt") {
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
	for _, operation := range []string{"filter", "delete", "update", "delete write", "update write"} {
		t.Run(operation, func(t *testing.T) {
			queue, err := appendstore.Open(filepath.Join(t.TempDir(), "queue.db"), appendstore.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = queue.Close() })
			s := &Storage{queue: queue}
			if addQueueErr := s.AddQueue(&Entry{InfoHash: "entry"}); addQueueErr != nil {
				t.Fatal(addQueueErr)
			}
			if !strings.HasSuffix(operation, "write") {
				if closeErr := queue.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			switch operation {
			case "filter":
				_, err = s.FilterQueued(nil)
			case "delete":
				err = s.DeleteWhereQueued(nil, nil)
			case "update":
				err = s.UpdateWhereQueued(nil, func(*Entry) bool { return true })
			case "delete write":
				err = s.DeleteWhereQueued(nil, func(*Entry) error { return queue.Close() })
			case "update write":
				err = s.UpdateWhereQueued(nil, func(*Entry) bool {
					if closeErr := queue.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					return true
				})
			}
			if !errors.Is(err, appendstore.ErrStoreClosed) {
				t.Fatalf("error = %v, want ErrStoreClosed", err)
			}
		})
	}
}

func TestQueueCleanupFailureRetainsEntry(t *testing.T) {
	queue, err := appendstore.Open(filepath.Join(t.TempDir(), "queue.db"), appendstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	s := &Storage{queue: queue}
	for _, hash := range []string{"keep", "delete"} {
		if addQueueErr := s.AddQueue(&Entry{InfoHash: hash}); addQueueErr != nil {
			t.Fatal(addQueueErr)
		}
	}
	cleanupErr := errors.New("cleanup failed")
	err = s.DeleteWhereQueued(nil, func(entry *Entry) error {
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
