package manager

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func twoFileEntry(key string, addedOn time.Time) *storage.Entry {
	e := testEntry(key)
	e.Name = "show"
	e.Files = map[string]*storage.File{
		"f1.mkv": {Name: "f1.mkv", Size: 1, InfoHash: key, AddedOn: addedOn},
		"f2.mkv": {Name: "f2.mkv", Size: 2, InfoHash: key, AddedOn: addedOn},
	}
	return e
}

func onlyItem(t *testing.T, store *storage.Storage) *storage.EntryItem {
	t.Helper()
	names := store.GetEntryItems()
	if len(names) != 1 {
		t.Fatalf("%d name index items, want 1", len(names))
	}
	for name := range names {
		item, err := store.GetEntryItem(name)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	return nil
}

func wantFile(t *testing.T, store *storage.Storage, key, file string, live bool) {
	t.Helper()
	e, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, fileErr := e.GetFile(file); (fileErr == nil) != live {
		t.Fatalf("entry file %s live=%v, want %v", file, fileErr == nil, live)
	}
	active := false
	for _, f := range e.GetActiveFiles() {
		active = active || f.Name == file
	}
	if active != live {
		t.Fatalf("entry active files list %s=%v, want %v", file, active, live)
	}
	if _, itemErr := onlyItem(t, store).GetFile(file); (itemErr == nil) != live {
		t.Fatalf("name index file %s live=%v, want %v", file, itemErr == nil, live)
	}
}

func removeAndRevive(t *testing.T, store *storage.Storage, m *Manager, key string, addedOn time.Time) {
	t.Helper()
	folder := onlyItem(t, store).Name
	if err := m.RemoveTorrentFile(folder, "f1.mkv"); err != nil {
		t.Fatal(err)
	}
	wantFile(t, store, key, "f1.mkv", false)
	wantFile(t, store, key, "f2.mkv", true)

	if err := store.PublishEntry(twoFileEntry(key, addedOn)); err != nil {
		t.Fatal(err)
	}
	wantFile(t, store, key, "f1.mkv", false)

	if err := store.PublishEntry(twoFileEntry(key, addedOn.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	wantFile(t, store, key, "f1.mkv", true)
}

func TestRemovedFileIsGoneForEveryReaderUntilRepairReAddsIt(t *testing.T) {
	t.Parallel()
	addedOn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	t.Run("no grab", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := newActorHarness(t, store)
			if err := store.AddOrUpdate(twoFileEntry("b", addedOn)); err != nil {
				t.Fatal(err)
			}
			removeAndRevive(t, store, h.libManager(), "b", addedOn)
		})
	})
	t.Run("grab", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := newActorHarness(t, store)
			h.grabbed()
			if err := store.AddOrUpdate(twoFileEntry("a", addedOn)); err != nil {
				t.Fatal(err)
			}
			removeAndRevive(t, store, h.libManager(), "a", addedOn)
		})
	})
}

func TestAnIndexTombstoneIsNotRevivedByASameAgeEntryWrite(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	addedOn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := store.AddOrUpdate(twoFileEntry("b", addedOn)); err != nil {
		t.Fatal(err)
	}
	item := onlyItem(t, store)
	item.Files["f1.mkv"].Deleted = true
	if err := store.UpdateItem(item); err != nil {
		t.Fatal(err)
	}
	if err := store.AddOrUpdate(twoFileEntry("b", addedOn)); err != nil {
		t.Fatal(err)
	}
	if _, err := onlyItem(t, store).GetFile("f1.mkv"); err == nil {
		t.Fatal("a same-age entry write revived a file the previous binary removed")
	}
}

func TestRemovingAFileOnlyTheNameIndexStillHasTombstonesIt(t *testing.T) {
	t.Parallel()
	addedOn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for name, already := range map[string]func(*storage.Entry){
		"absent":  func(e *storage.Entry) { delete(e.Files, "f1.mkv") },
		"removed": func(e *storage.Entry) { e.Files["f1.mkv"].Deleted = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				removeIndexOnlyFile(t, store, newActorHarness(t, store).libManager(), addedOn, already)
			})
		})
	}
}

func removeIndexOnlyFile(
	t *testing.T,
	store *storage.Storage,
	m *Manager,
	addedOn time.Time,
	already func(*storage.Entry),
) {
	t.Helper()
	if err := store.AddOrUpdate(twoFileEntry("b", addedOn)); err != nil {
		t.Fatal(err)
	}
	e, err := store.Get("b")
	if err != nil {
		t.Fatal(err)
	}
	already(e)
	if err = store.AddOrUpdate(e); err != nil {
		t.Fatal(err)
	}
	item := onlyItem(t, store)
	item.Files["f1.mkv"].Deleted = false
	if err = store.UpdateItem(item); err != nil {
		t.Fatal(err)
	}
	if err = m.RemoveTorrentFile(item.Name, "f1.mkv"); err != nil {
		t.Fatal(err)
	}
	if _, itemErr := onlyItem(t, store).GetFile("f1.mkv"); itemErr == nil {
		t.Fatal("the name index still serves a removed file")
	}
}

func TestRemovingOneEntrysFileKeepsAnotherEntrysSameNamedFile(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	addedOn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := store.AddOrUpdate(twoFileEntry("b", addedOn)); err != nil {
		t.Fatal(err)
	}
	other := twoFileEntry("c", addedOn)
	for _, f := range other.Files {
		f.Size = onlyItem(t, store).Files[f.Name].Size
	}
	other.Files["f1.mkv"].Deleted = true
	if err := store.AddOrUpdate(other); err != nil {
		t.Fatal(err)
	}
	f, err := onlyItem(t, store).GetFile("f1.mkv")
	if err != nil {
		t.Fatalf("another entry's removal hid this entry's file: %v", err)
	}
	if f.InfoHash != "b" {
		t.Fatalf("name index serves %s's f1.mkv, want b's", f.InfoHash)
	}
}
