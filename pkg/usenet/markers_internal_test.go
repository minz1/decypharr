package usenet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A failed NZB without a source path has no marker to remove. The marker
// root must not turn the empty path into a bare ".processing" and delete an
// unrelated file of that name.
func TestMarkAsFailedWithoutPathLeavesMarkerDirectoryAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	markers, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = markers.Close() })
	unrelated := filepath.Join(dir, ".processing")
	if writeErr := os.WriteFile(unrelated, nil, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	u := &Usenet{
		nzbStorage:  &NZBStorage{metaDir: t.TempDir(), codec: testCodec(t), logger: zerolog.Nop()},
		logger:      zerolog.Nop(),
		metadataDir: dir,
		markers:     markers,
	}
	if markErr := u.markAsFailed(&storage.NZB{ID: "queued"}, errors.New("boom")); markErr != nil {
		t.Fatal(markErr)
	}
	if _, statErr := os.Stat(unrelated); statErr != nil {
		t.Fatalf("unrelated .processing was removed: %v", statErr)
	}
}

// Markers live next to their NZB source in the metadata directory, and a
// source outside it gets none.
func TestProcessingMarkerStaysInMetadataDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	markers, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = markers.Close() })
	u := &Usenet{logger: zerolog.Nop(), metadataDir: dir, markers: markers}

	source := filepath.Join(dir, "id.nzb")
	if markErr := u.markAsProcessing(&storage.NZB{ID: "id", Path: source}); markErr != nil {
		t.Fatal(markErr)
	}
	if data, readErr := os.ReadFile(source + ".processing"); readErr != nil || string(data) != "id" {
		t.Fatalf("marker = %q, %v", data, readErr)
	}
	u.removeProcessingMarker(source)
	if _, statErr := os.Stat(source + ".processing"); !os.IsNotExist(statErr) {
		t.Fatalf("marker not removed: %v", statErr)
	}

	outside := filepath.Join(t.TempDir(), "id.nzb")
	if markErr := u.markAsProcessing(&storage.NZB{ID: "id", Path: outside}); markErr == nil {
		t.Fatal("marked a source outside the metadata directory")
	}
	if _, statErr := os.Stat(outside + ".processing"); !os.IsNotExist(statErr) {
		t.Fatalf("marker written outside the metadata directory: %v", statErr)
	}
}
