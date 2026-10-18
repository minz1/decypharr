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

// A job that fails after parsing (e.g. hearsay rejected it) must not leave its .nzb, marker and parsing record behind.
func TestFailReleasesAParsedNZB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	markers, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = markers.Close() })
	u := &Usenet{
		nzbStorage:  &NZBStorage{metaDir: t.TempDir(), codec: testCodec(t), logger: zerolog.Nop()},
		logger:      zerolog.Nop(),
		metadataDir: dir,
		markers:     markers,
	}
	source := filepath.Join(dir, "id.nzb")
	if writeErr := os.WriteFile(source, []byte("<nzb/>"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	nzb := &storage.NZB{ID: "id", Path: source, Status: NZBStatusParsing}
	if markErr := u.markAsProcessing(nzb); markErr != nil {
		t.Fatal(markErr)
	}
	if addErr := u.nzbStorage.AddNZB(nzb); addErr != nil {
		t.Fatal(addErr)
	}

	if failErr := u.Fail("id", errors.New("hearsay claims segments missing")); failErr != nil {
		t.Fatal(failErr)
	}
	for _, path := range []string{source, source + ".processing"} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("%s still on disk: %v", path, statErr)
		}
	}
	if header, getErr := u.nzbStorage.GetNZBHeader("id"); getErr != nil || header.Status != NZBStatusFailed {
		t.Fatalf("record = %+v, %v; want failed", header, getErr)
	}
	if missingErr := u.Fail("never-parsed", errors.New("x")); missingErr != nil {
		t.Fatalf("Fail(unknown id) = %v, want nil", missingErr)
	}
}

// A completed NZB whose post-download step failed still streams: Fail must not rewrite it from a segment-less header.
func TestFailLeavesACompletedNZBStreamable(t *testing.T) {
	t.Parallel()
	nzbStorage := &NZBStorage{metaDir: t.TempDir(), codec: testCodec(t), logger: zerolog.Nop()}
	u := &Usenet{nzbStorage: nzbStorage, logger: zerolog.Nop()}
	completed := &storage.NZB{ID: "done", Status: NZBStatusCompleted, Files: []storage.NZBFile{{
		Name: "movie.mkv", Size: 10,
		Segments: []storage.NZBSegment{{Number: 1, MessageID: "seg@x", Bytes: 10, EndOffset: 9}},
	}}}
	if addErr := u.nzbStorage.AddNZB(completed); addErr != nil {
		t.Fatal(addErr)
	}
	if failErr := u.Fail("done", errors.New("timeout waiting for mount files")); failErr != nil {
		t.Fatal(failErr)
	}
	file, err := u.nzbStorage.GetNZBFile("done", "movie.mkv")
	if err != nil || len(file.Segments) != 1 {
		t.Fatalf("file = %+v, %v; want its segment intact", file, err)
	}
	if header, _ := u.nzbStorage.GetNZBHeader("done"); header.Status != NZBStatusCompleted {
		t.Fatalf("status = %s, want completed", header.Status)
	}
}
