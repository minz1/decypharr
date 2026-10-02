package storage_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestGetTorrentFolderArrSubmittedNameFromMagnet(t *testing.T) {
	t.Parallel()
	entry := &storage.Entry{
		InfoHash: "8a19577fb5f690970ca43a57ff1011ae202244b8",
		Name:     "provider-name",
		Magnet:   "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=Example+Show+Season+01+S01+1080p+WEB-DL+x265",
	}

	got := storage.GetTorrentFolder(config.WebDavUseArrSubmittedName, entry)
	want := "Example Show Season 01 S01 1080p WEB-DL x265"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestGetTorrentFolderArrSubmittedNameSanitizesPathUnsafeNames(t *testing.T) {
	t.Parallel()
	entry := &storage.Entry{
		InfoHash: "8a19577fb5f690970ca43a57ff1011ae202244b8",
		Name:     "provider-name",
		Magnet:   "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=..%2Fbad%3Aname%3F",
	}

	got := storage.GetTorrentFolder(config.WebDavUseArrSubmittedName, entry)
	want := "badname"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestGetTorrentFolderArrSubmittedNameFallsBackToInfoHash(t *testing.T) {
	t.Parallel()
	entry := &storage.Entry{
		InfoHash: "8a19577fb5f690970ca43a57ff1011ae202244b8",
		Name:     "provider-name",
		Magnet:   "magnet:?xt=urn:btih:8a19577fb5f690970ca43a57ff1011ae202244b8&dn=..%2F%3F",
	}

	got := storage.GetTorrentFolder(config.WebDavUseArrSubmittedName, entry)
	want := "8a19577fb5f690970ca43a57ff1011ae202244b8"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestGetFirstFileReturnsOnlyActiveFiles(t *testing.T) {
	t.Parallel()
	active := &storage.File{Name: "active"}
	item := &storage.EntryItem{Files: map[string]*storage.File{"deleted": {Deleted: true}, "nil": nil}}
	if file, err := item.GetFirstFile(); file != nil || err == nil {
		t.Fatalf("no active files: got %v, %v", file, err)
	}
	item.Files["active"] = active
	if file, err := item.GetFirstFile(); file != active || err != nil {
		t.Fatalf("active file: got %v, %v", file, err)
	}
}
