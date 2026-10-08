package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/kvstore"

	"github.com/sirrobot01/appendstore"
	"google.golang.org/protobuf/proto"
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

// Only entries whose folder could match are decoded: a queued record that
// cannot be a candidate is skipped by its metadata, even if its value is
// unreadable. The season-pack fan-out used to decode the whole queue.
func TestFilterQueuedByFolderDecodesOnlyCandidates(t *testing.T) {
	t.Parallel()
	s, err := NewStorage(filepath.Join(t.TempDir(), "db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	candidate := &Entry{InfoHash: strings.Repeat("a", 40), Name: "Show S01"}
	if addErr := s.AddQueue(candidate); addErr != nil {
		t.Fatal(addErr)
	}
	// Written under hash naming: its folder is its ID, so it stays a candidate.
	hashNamed := &Entry{InfoHash: strings.Repeat("b", 32), Name: "Show S02"}
	if putErr := s.queue.Put(hashNamed.InfoHash, mustMarshalEntry(t, hashNamed),
		&appendstore.PutOptions{Attributes: map[string]string{attributeName: hashNamed.InfoHash}}); putErr != nil {
		t.Fatal(putErr)
	}
	// Another show with an unreadable value: never decoded.
	if putErr := s.queue.Put("other", []byte("not a protobuf \xff\xff"),
		&appendstore.PutOptions{Attributes: map[string]string{attributeName: "Other Show"}}); putErr != nil {
		t.Fatal(putErr)
	}

	folders := map[string]struct{}{"Show S01": {}}
	entries, err := s.FilterQueuedByFolder(folders, nil)
	if err != nil {
		t.Fatalf("FilterQueuedByFolder decoded a non-candidate: %v", err)
	}
	got := make(map[string]bool, len(entries))
	for _, entry := range entries {
		got[entry.Name] = true
	}
	if len(entries) != 2 || !got["Show S01"] || !got["Show S02"] {
		t.Fatalf("entries = %v, want Show S01 and the hash-named Show S02", got)
	}
}

func mustMarshalEntry(t *testing.T, entry *Entry) []byte {
	t.Helper()
	data, err := proto.Marshal(EntryToProto(entry))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
