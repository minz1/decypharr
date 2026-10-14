package storage

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/appendstore"
)

// newIndexedTestStorage opens a store holding one entry in folder "folder".
func newIndexedTestStorage(t *testing.T) (*Storage, *Entry) {
	t.Helper()
	s, err := NewStorage(t.TempDir(), Options{})
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

func TestDeleteEntryKeepsOtherEntriesFiles(t *testing.T) {
	t.Parallel()
	old := time.Unix(1_000, 0)
	cases := map[string]struct {
		deleted, other *Entry
		wantFolder     string
		wantItem       bool
	}{
		"split child removes its folder item": {
			deleted: &Entry{InfoHash: "child", Name: "child-folder", Files: map[string]*File{
				"a.mkv": {Name: "a.mkv", InfoHash: "parent", Size: 1, AddedOn: old},
			}},
			wantFolder: "child-folder",
		},
		"hidden duplicate leaves the visible item": {
			deleted: &Entry{InfoHash: "old", Name: "folder", Files: map[string]*File{
				"a.mkv": {Name: "a.mkv", InfoHash: "old", Size: 1, AddedOn: old},
			}},
			other: &Entry{InfoHash: "new", Name: "folder", Files: map[string]*File{
				"a.mkv": {Name: "a.mkv", InfoHash: "new", Size: 2, AddedOn: old.Add(time.Hour)},
			}},
			wantFolder: "folder",
			wantItem:   true,
		},
		"normal entry removes its folder item": {
			deleted: &Entry{InfoHash: "solo", Name: "folder", Files: map[string]*File{
				"a.mkv": {Name: "a.mkv", InfoHash: "solo", Size: 1, AddedOn: old},
			}},
			wantFolder: "folder",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, err := NewStorage(t.TempDir(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			for _, e := range []*Entry{tc.other, tc.deleted} {
				if e == nil {
					continue
				}
				if err = s.AddOrUpdate(e); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Delete(tc.deleted.InfoHash); err != nil {
				t.Fatal(err)
			}
			assertFolderItem(t, s, tc.wantFolder, tc.wantItem)
		})
	}
}

func assertFolderItem(t *testing.T, s *Storage, folder string, want bool) {
	t.Helper()
	item, err := s.GetEntryItem(folder)
	switch {
	case want && (err != nil || item.Files["a.mkv"].InfoHash != "new"):
		t.Fatalf("visible item lost: %v %+v", err, item)
	case !want && !errors.Is(err, appendstore.ErrKeyNotFound):
		t.Fatalf("folder item survived: %v %+v", err, item)
	}
}

func TestDeleteEntryNeverMatchesEmptyFileHashes(t *testing.T) {
	t.Parallel()
	s, err := NewStorage(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	old := time.Unix(1_000, 0)
	for _, e := range []*Entry{
		{InfoHash: "new", Name: "folder", Files: map[string]*File{"a.mkv": {Name: "a.mkv", Size: 2, AddedOn: old.Add(time.Hour)}}},
		{InfoHash: "old", Name: "folder", Files: map[string]*File{"a.mkv": {Name: "a.mkv", Size: 1, AddedOn: old}}},
	} {
		if err = s.AddOrUpdate(e); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Delete("old"); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetEntryItem("folder")
	if err != nil || item.Files["a.mkv"] == nil || item.Files["a.mkv"].Size != 2 {
		t.Fatalf("visible file lost: %v %+v", err, item)
	}
}
