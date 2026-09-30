package storage

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/sirrobot01/appendstore"
)

// newIndexedTestStorage opens a store holding one entry in folder "folder".
func newIndexedTestStorage(t *testing.T) (*Storage, *Entry) {
	t.Helper()
	s, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	entry := &Entry{
		InfoHash: "hash",
		Name:     "folder",
		Files:    map[string]*File{"file": {Name: "file", InfoHash: "hash", Size: 10}},
	}
	if addOrUpdateErr := s.AddOrUpdate(entry); addOrUpdateErr != nil {
		t.Fatal(addOrUpdateErr)
	}
	return s, entry
}

// breakEntryItems corrupts the folder's index record or closes the index store.
func breakEntryItems(t *testing.T, s *Storage, failure string) {
	t.Helper()
	if failure == "corrupt" {
		if putErr := s.entryItems.Put("folder", []byte{0xff}, nil); putErr != nil {
			t.Fatal(putErr)
		}
		return
	}
	if closeErr := s.entryItems.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestEntryMutationsReportIndexFailures(t *testing.T) {
	t.Parallel()
	operations := map[string]func(*Storage, *Entry) error{
		"add":         (*Storage).AddOrUpdate,
		"delete":      func(s *Storage, entry *Entry) error { return s.Delete(entry.InfoHash) },
		"update item": (*Storage).updateEntryItem,
	}
	for name, operation := range operations {
		for _, failure := range []string{"corrupt", "closed"} {
			t.Run(name+"/"+failure, func(t *testing.T) {
				t.Parallel()
				s, entry := newIndexedTestStorage(t)
				breakEntryItems(t, s, failure)

				assertIndexFailureReported(t, s, entry, failure, operation(s, entry))
			})
		}
	}
}

// assertIndexFailureReported checks that a mutation surfaced the broken index
// and left both the source entry and a corrupt index record untouched.
func assertIndexFailureReported(t *testing.T, s *Storage, entry *Entry, failure string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "folder") {
		t.Fatalf("error = %v, want folder context", err)
	}
	if failure == "closed" && !errors.Is(err, appendstore.ErrStoreClosed) {
		t.Fatalf("error = %v, want ErrStoreClosed", err)
	}
	if _, getErr := s.Get(entry.InfoHash); getErr != nil {
		t.Fatalf("source entry was lost: %v", getErr)
	}
	if failure != "corrupt" {
		return
	}
	data, getErr := s.entryItems.Get("folder")
	if getErr != nil || !bytes.Equal(data, []byte{0xff}) {
		t.Fatalf("corrupt index was overwritten: %x, %v", data, getErr)
	}
}

func TestEntryWriteFailureLeavesIndexUnchanged(t *testing.T) {
	t.Parallel()
	s, entry := newIndexedTestStorage(t)
	before, err := s.entryItems.Get("folder")
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := s.entries.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	entry.Files["file"].Size = 20
	if addOrUpdateErr := s.AddOrUpdate(entry); !errors.Is(addOrUpdateErr, appendstore.ErrStoreClosed) {
		t.Fatalf("error = %v, want ErrStoreClosed", addOrUpdateErr)
	}
	after, err := s.entryItems.Get("folder")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("index changed after failed source write: %v", err)
	}
}

func TestEntryMutationReportsHealthFailure(t *testing.T) {
	t.Parallel()
	operations := map[string]func(*Storage, *Entry) error{
		"add": func(s *Storage, entry *Entry) error {
			entry.Files["file"].Size = 20
			return s.AddOrUpdate(entry)
		},
		"delete": func(s *Storage, entry *Entry) error { return s.Delete(entry.InfoHash) },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, entry := newIndexedTestStorage(t)
			if closeErr := s.repairState.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err := operation(s, entry); !errors.Is(err, appendstore.ErrStoreClosed) {
				t.Fatalf("error = %v, want ErrStoreClosed", err)
			}
			item, err := s.GetEntryItem("folder")
			if err != nil || item.Files["file"].Size != 10 {
				t.Fatalf("index changed after health failure: %v, %v", item, err)
			}
		})
	}
}
