package storage

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/sirrobot01/appendstore"
)

func TestEntryMutationsReportIndexFailures(t *testing.T) {
	for _, operation := range []string{"add", "delete", "update item"} {
		for _, failure := range []string{"corrupt", "closed"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
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
				if failure == "corrupt" {
					if putErr := s.entryItems.Put("folder", []byte{0xff}, nil); putErr != nil {
						t.Fatal(putErr)
					}
				} else if closeErr := s.entryItems.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				switch operation {
				case "add":
					err = s.AddOrUpdate(entry)
				case "delete":
					err = s.Delete(entry.InfoHash)
				case "update item":
					err = s.updateEntryItem(entry)
				}
				if err == nil || !strings.Contains(err.Error(), "folder") {
					t.Fatalf("error = %v, want folder context", err)
				}
				if failure == "closed" && !errors.Is(err, appendstore.ErrStoreClosed) {
					t.Fatalf("error = %v, want ErrStoreClosed", err)
				}
				if _, getErr := s.Get(entry.InfoHash); getErr != nil {
					t.Fatalf("source entry was lost: %v", getErr)
				}
				if failure == "corrupt" {
					data, getErr := s.entryItems.Get("folder")
					if getErr != nil || !bytes.Equal(data, []byte{0xff}) {
						t.Fatalf("corrupt index was overwritten: %x, %v", data, getErr)
					}
				}
			})
		}
	}
}

func TestEntryWriteFailureLeavesIndexUnchanged(t *testing.T) {
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
	for _, operation := range []string{"add", "delete"} {
		t.Run(operation, func(t *testing.T) {
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
			if closeErr := s.repairState.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if operation == "add" {
				entry.Files["file"].Size = 20
				err = s.AddOrUpdate(entry)
			} else {
				err = s.Delete(entry.InfoHash)
			}
			if !errors.Is(err, appendstore.ErrStoreClosed) {
				t.Fatalf("error = %v, want ErrStoreClosed", err)
			}
			item, err := s.GetEntryItem("folder")
			if err != nil || item.Files["file"].Size != 10 {
				t.Fatalf("index changed after health failure: %v, %v", item, err)
			}
		})
	}
}
