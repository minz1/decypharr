package storage_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// storage.File IDs must be assigned once and survive entries being rebuilt from
// provider responses — the .strm URLs written from them live for years.
func TestFileIDsAreStableAcrossUpdates(t *testing.T) {
	t.Parallel()
	s, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	infohash := "aabbccddeeff00112233445566778899aabbccdd"
	entry := &storage.Entry{
		InfoHash: infohash,
		Name:     "Movie.2023",
		Files: map[string]*storage.File{
			"Movie.2023.mkv": {Name: "Movie.2023.mkv", Size: 100, InfoHash: infohash},
		},
	}
	if addOrUpdateErr := s.AddOrUpdate(entry); addOrUpdateErr != nil {
		t.Fatal(addOrUpdateErr)
	}
	id := entry.Files["Movie.2023.mkv"].ID
	if id == "" {
		t.Fatal("AddOrUpdate did not assign a file ID")
	}

	// Same entry rebuilt without IDs, plus a new file.
	rebuilt := &storage.Entry{
		InfoHash: infohash,
		Name:     "Movie.2023",
		Files: map[string]*storage.File{
			"Movie.2023.mkv": {Name: "Movie.2023.mkv", Size: 100, InfoHash: infohash},
			"Movie.2023.srt": {Name: "Movie.2023.srt", Size: 10, InfoHash: infohash},
		},
	}
	if addOrUpdateErr := s.AddOrUpdate(rebuilt); addOrUpdateErr != nil {
		t.Fatal(addOrUpdateErr)
	}
	if got := rebuilt.Files["Movie.2023.mkv"].ID; got != id {
		t.Fatalf("existing file ID changed: %q -> %q", id, got)
	}
	if rebuilt.Files["Movie.2023.srt"].ID == "" {
		t.Fatal("new file did not get an ID")
	}

	// IDs are persisted and resolvable.
	loaded, err := s.Get(infohash)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Files["Movie.2023.mkv"].ID; got != id {
		t.Fatalf("persisted ID = %q, want %q", got, id)
	}
	file, err := loaded.GetFileByID(id)
	if err != nil || file.Name != "Movie.2023.mkv" {
		t.Fatalf("GetFileByID(%q) = %v, %v", id, file, err)
	}
}
