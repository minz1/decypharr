package storage_test

import (
	"path/filepath"
	"strings"
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

// Provider-supplied names never turn a folder into a path that leaves its
// parent; ordinary names are kept as they are.
func TestGetTorrentFolderIsOneElement(t *testing.T) {
	t.Parallel()
	const hash = "8a19577fb5f690970ca43a57ff1011ae202244b8"
	namings := []config.WebDavFolderNaming{
		config.WebDavUseFileName, config.WebDavUseOriginalName, config.WebDavUseFileNameNoExt,
		config.WebDavUseOriginalNameNoExt, config.WebDavUseArrSubmittedName, config.WebdavUseHash, "",
	}
	for _, name := range []string{"..", ".", "", "../../etc", "Show/Season 1", "a/../.."} {
		entry := &storage.Entry{InfoHash: hash, Name: name, OriginalFilename: name, SavePath: "/downloads"}
		for _, naming := range namings {
			folder := storage.GetTorrentFolder(naming, entry)
			if folder == "" || folder == "." || folder == ".." || strings.ContainsRune(folder, '/') {
				t.Errorf("GetTorrentFolder(%q, %q) = %q, want one path element", naming, name, folder)
			}
			if dir := entry.DownloadPath(naming); filepath.Dir(dir) != filepath.Clean("/downloads") {
				t.Errorf("DownloadPath(%q, %q) = %q, want a child of /downloads", naming, name, dir)
			}
		}
	}
	entry := &storage.Entry{InfoHash: hash, Name: "Movie: Part 1 (2023).mkv"}
	if got := storage.GetTorrentFolder(config.WebDavUseFileName, entry); got != entry.Name {
		t.Fatalf("ordinary name changed to %q", got)
	}
}
