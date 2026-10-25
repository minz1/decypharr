package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
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
		"visible duplicate hands the folder to the hidden one": {
			deleted: &Entry{InfoHash: "newest", Name: "folder", Files: map[string]*File{
				"a.mkv": {Name: "a.mkv", InfoHash: "newest", Size: 3, AddedOn: old.Add(2 * time.Hour)},
			}},
			other: &Entry{InfoHash: "new", Name: "folder", Files: map[string]*File{
				"a.mkv": {Name: "a.mkv", InfoHash: "new", Size: 2, AddedOn: old},
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

// newSharedFolderStorage stores entries that all render folder "folder", in order.
func newSharedFolderStorage(t *testing.T, entries ...*Entry) *Storage {
	t.Helper()
	s, err := NewStorage(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, e := range entries {
		if err = s.AddOrUpdate(e); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func sharedEntry(hash string, size int64, addedOn time.Time) *Entry {
	return &Entry{InfoHash: hash, Name: "folder", Files: map[string]*File{
		"a.mkv": {Name: "a.mkv", InfoHash: hash, Size: size, AddedOn: addedOn},
	}}
}

func TestDeleteRefillsSharedFolderWithNewestCopy(t *testing.T) {
	t.Parallel()
	old := time.Unix(1_000, 0)
	var entries []*Entry
	for i := range 5 {
		entries = append(
			entries,
			sharedEntry(fmt.Sprintf("a-older%d", i), int64(i+1), old.Add(time.Duration(i)*time.Minute)),
		)
	}
	entries = append(
		entries,
		sharedEntry("z-newest-left", 50, old.Add(time.Hour)),
		sharedEntry("deleted", 99, old.Add(2*time.Hour)),
	)
	s := newSharedFolderStorage(t, entries...)

	if err := s.Delete("deleted"); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetEntryItem("folder")
	if err != nil || item.Files["a.mkv"].InfoHash != "z-newest-left" || item.Size != 50 {
		t.Fatalf("folder item = %+v, %v; want the newest remaining copy", item, err)
	}
}

func TestDeleteSkipsCorruptedSharedEntry(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s, err := NewStorage(t.TempDir(), Options{Logger: zerolog.New(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	old := time.Unix(1_000, 0)
	for _, e := range []*Entry{sharedEntry("unreadable-sibling", 1, old), sharedEntry("deleted", 2, old.Add(time.Hour))} {
		if err = s.AddOrUpdate(e); err != nil {
			t.Fatal(err)
		}
	}
	corruptStoredRecord(t, filepath.Join(s.dir, "entries.db"), "unreadable-sibling")

	if err = s.Delete("deleted"); err != nil {
		t.Fatalf("delete blocked by a corrupted sibling: %v", err)
	}
	if _, getErr := s.Get("deleted"); !errors.Is(getErr, appendstore.ErrKeyNotFound) {
		t.Fatalf("entry survived the delete: %v", getErr)
	}
	if _, itemErr := s.GetEntryItem("folder"); !errors.Is(itemErr, appendstore.ErrKeyNotFound) {
		t.Fatalf("folder item survived: %v", itemErr)
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) || !strings.Contains(logs.String(), "unreadable-sibling") {
		t.Fatalf("logs = %q, want a warning naming the corrupted sibling", logs.String())
	}
}

func TestDeleteSkipsUndecodableSharedEntry(t *testing.T) {
	t.Parallel()
	s := newSharedFolderStorage(t, sharedEntry("deleted", 2, time.Unix(1_000, 0)))
	if err := s.entries.Put(
		"undecodable",
		[]byte{0xff},
		s.entryPutOptions(&Entry{InfoHash: "undecodable", Name: "folder"}),
	); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete("deleted"); err != nil {
		t.Fatalf("delete blocked by an undecodable sibling: %v", err)
	}
	if _, err := s.GetEntryItem("folder"); !errors.Is(err, appendstore.ErrKeyNotFound) {
		t.Fatalf("folder item survived: %v", err)
	}
}

func TestRemoveFromEntryItemReportsEntryListFailure(t *testing.T) {
	t.Parallel()
	s := newSharedFolderStorage(t, sharedEntry("deleted", 2, time.Unix(1_000, 0)))
	entry, err := s.Get("deleted")
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := s.entries.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	if err = s.removeFromEntryItem(entry); !errors.Is(err, appendstore.ErrStoreClosed) {
		t.Fatalf("error = %v, want ErrStoreClosed", err)
	}
	if item, getErr := s.GetEntryItem("folder"); getErr != nil || item.Files["a.mkv"] == nil {
		t.Fatalf("folder item changed: %+v, %v", item, getErr)
	}
}

// corruptStoredRecord flips a byte inside the stored record that holds marker, so its checksum fails.
func corruptStoredRecord(t *testing.T, storePath, marker string) {
	t.Helper()
	found := false
	walkErr := filepath.WalkDir(storePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		at := bytes.LastIndex(data, []byte(marker))
		if at < 0 {
			return nil
		}
		found = true
		f, openErr := os.OpenFile(path, os.O_WRONLY, 0)
		if openErr != nil {
			return openErr
		}
		defer f.Close()
		_, writeErr := f.WriteAt([]byte{data[at] ^ 0xff}, int64(at))
		return writeErr
	})
	if walkErr != nil || !found {
		t.Fatalf("corrupt %q in %s: found=%v, %v", marker, storePath, found, walkErr)
	}
}

func TestDeleteLeavesUnownedTiedFileAlone(t *testing.T) {
	t.Parallel()
	tie := time.Unix(1_000, 0)
	b := sharedEntry("b", 10, tie)
	c := sharedEntry("c", 20, tie)
	d := &Entry{InfoHash: "d", Name: "folder", Files: map[string]*File{
		"b.mkv": {Name: "b.mkv", InfoHash: "d", Size: 5, AddedOn: tie},
	}}
	s := newSharedFolderStorage(t, b, c, d)
	before, err := s.GetEntryItem("folder")
	if err != nil {
		t.Fatal(err)
	}
	want := before.Files["a.mkv"].InfoHash

	if err = s.Delete("d"); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetEntryItem("folder")
	if err != nil || item.Files["a.mkv"].InfoHash != want || item.Files["b.mkv"] != nil {
		t.Fatalf("folder item = %+v, %v; want a.mkv from %q and no b.mkv", item, err, want)
	}
}
