package usenet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// newMarkedUsenet returns a Usenet with real storage and a source .nzb plus its
// .processing marker in the metadata directory.
func newMarkedUsenet(t *testing.T, id string) (*Usenet, string) {
	t.Helper()
	dir := t.TempDir()
	markers, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = markers.Close() })
	metaDir := filepath.Join(t.TempDir(), "meta")
	nzbStorage, err := NewNZBStorage(metaDir, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	u := &Usenet{nzbStorage: nzbStorage, logger: zerolog.Nop(), metadataDir: dir, markers: markers}
	source := filepath.Join(dir, id+".nzb")
	if writeErr := os.WriteFile(source, []byte("<nzb/>"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if markErr := u.markAsProcessing(&storage.NZB{ID: id, Path: source}); markErr != nil {
		t.Fatal(markErr)
	}
	return u, source
}

// blockMetaWrites makes AddNZB for id fail by occupying the temp path its
// atomic writer uses with a directory.
func blockMetaWrites(t *testing.T, nzbStorage *NZBStorage, id string) func() {
	t.Helper()
	path, err := nzbStorage.metaFilePath(id)
	if err != nil {
		t.Fatal(err)
	}
	blocker := path + ".tmp"
	if mkdirErr := os.Mkdir(blocker, 0o750); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	return func() {
		if removeErr := os.Remove(blocker); removeErr != nil {
			t.Fatal(removeErr)
		}
	}
}

// markAsCompleted must persist Completed before it removes the source and the
// marker. A write failure used to leave the persisted record "parsing" with its
// Path pointing at a deleted file, so a restart could never re-drive the NZB.
func TestMarkAsCompletedKeepsSourceWhenPersistFails(t *testing.T) {
	t.Parallel()
	u, source := newMarkedUsenet(t, "nzb-1")
	marker := source + ".processing"
	nzb := &storage.NZB{ID: "nzb-1", Name: "release", Path: source, Status: NZBStatusParsing}
	if err := u.nzbStorage.AddNZB(nzb); err != nil {
		t.Fatal(err)
	}

	unblock := blockMetaWrites(t, u.nzbStorage, nzb.ID)
	if err := u.markAsCompleted(nzb); err == nil {
		t.Fatal("markAsCompleted() = nil, want the persist error")
	}

	for _, p := range []string{source, marker} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed although Completed was not persisted: %v", filepath.Base(p), err)
		}
	}
	if nzb.Path != source {
		t.Errorf("Path = %q, want %q kept for the retry", nzb.Path, source)
	}
	if nzb.Status != NZBStatusParsing {
		t.Errorf("in-memory Status = %q, want %q", nzb.Status, NZBStatusParsing)
	}
	header, err := u.nzbStorage.GetNZBHeader(nzb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if header.Status != NZBStatusParsing || header.Path != source {
		t.Fatalf("persisted status/path = %q/%q, want parsing/%q", header.Status, header.Path, source)
	}

	// The NZB is still parsing, so a later Fail reaches failed and releases the files.
	unblock()
	if failErr := u.Fail(nzb.ID, errors.New("later failure")); failErr != nil {
		t.Fatalf("Fail() = %v", failErr)
	}
	header, err = u.nzbStorage.GetNZBHeader(nzb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if header.Status != NZBStatusFailed {
		t.Fatalf("persisted status = %q, want failed", header.Status)
	}
	for _, p := range []string{source, marker} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("%s still on disk after Fail: %v", filepath.Base(p), statErr)
		}
	}
}

// The success path still cleans up, but only after the record is durable.
func TestMarkAsCompletedRemovesSourceAfterPersist(t *testing.T) {
	t.Parallel()
	u, source := newMarkedUsenet(t, "nzb-2")
	nzb := &storage.NZB{ID: "nzb-2", Name: "release", Path: source, Status: NZBStatusParsing}

	if err := u.markAsCompleted(nzb); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{source, source + ".processing"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after completion (err=%v)", filepath.Base(p), err)
		}
	}
	if nzb.Path != "" || nzb.Status != NZBStatusCompleted {
		t.Errorf("path/status = %q/%q, want empty/completed", nzb.Path, nzb.Status)
	}
	header, err := u.nzbStorage.GetNZBHeader(nzb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if header.Status != NZBStatusCompleted || header.Path != "" {
		t.Fatalf("persisted status/path = %q/%q, want completed/empty", header.Status, header.Path)
	}
}
