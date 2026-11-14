package manager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestQueueDeleteRemovesFiles(t *testing.T) {
	t.Parallel()
	queue, entry, downloadedPath := newQueueDeleteTest(t)

	if err := queue.deleteEntryFiles(entry); err != nil {
		t.Fatalf("delete entry files: %v", err)
	}
	if _, err := os.Stat(downloadedPath); !os.IsNotExist(err) {
		t.Fatalf("expected downloaded files to be removed, got %v", err)
	}
}

// A record without an absolute save path names no download: deleting its files
// once removed a directory relative to the process's working directory.
func TestQueueDeleteKeepsARelativeDownloadPath(t *testing.T) {
	t.Parallel()
	queue, entry, downloadedPath := newQueueDeleteTest(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, entry.SavePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = queue.deleteEntryFiles(
		&storage.Entry{InfoHash: entry.InfoHash, Name: entry.Name, SavePath: rel},
	); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(downloadedPath); err != nil {
		t.Fatalf("a relative download path was deleted: %v", err)
	}
}

func newQueueDeleteTest(t *testing.T) (*Queue, *storage.Entry, string) {
	t.Helper()

	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close storage: %v", closeErr)
		}
	})

	savePath := filepath.Join(t.TempDir(), "downloads")
	entry := &storage.Entry{
		InfoHash: "0123456789abcdef",
		Name:     "Example.mkv",
		SavePath: savePath,
	}
	downloadedPath := entry.DownloadPath("")
	if mkdirAllErr := os.MkdirAll(downloadedPath, 0o755); mkdirAllErr != nil {
		t.Fatalf("create downloaded path: %v", mkdirAllErr)
	}
	if writeFileErr := os.WriteFile(
		filepath.Join(downloadedPath, "video.mkv"),
		[]byte("test"),
		0o644,
	); writeFileErr != nil {
		t.Fatalf("create downloaded file: %v", writeFileErr)
	}

	queue := newQueue(store, nil, &grabIndex{}, zerolog.Nop())
	if addErr := queue.Add(entry); addErr != nil {
		t.Fatalf("add queued entry: %v", addErr)
	}
	return queue, entry, downloadedPath
}

// Removing a failed NZB entry must remove its staged file too; it once leaked one .queued file per failure.
func TestCleanupRemovesStagedNZB(t *testing.T) {
	t.Parallel()
	staged := filepath.Join(t.TempDir(), "job.queued")
	if writeErr := os.WriteFile(staged, []byte("<nzb/>"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	m := &Manager{queue: &Queue{logger: zerolog.Nop()}, logger: zerolog.Nop()}
	entry := &storage.Entry{InfoHash: "job", Name: "failed", Protocol: config.ProtocolNZB, Magnet: staged}

	got := m.cleanupChild(t.Context(), entryChildCall{kind: grab.ChildCleanup, grab: grab.Grab{Entry: entry}})
	if got.ev == nil || got.ev.Kind != grab.EvCleanupDone {
		t.Fatalf("cleanup result %+v, want done", got)
	}
	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatalf("staged NZB still on disk after the cleanup: %v", statErr)
	}
}

// An entry whose grab is cleaned up forgets its provider wait: the map stays bounded, and a later grab of the same
// hash logs its wait's start at WARN again.
func TestCleanupForgetsTheProviderWait(t *testing.T) {
	t.Parallel()
	m := &Manager{queue: newQueue(nil, nil, &grabIndex{}, zerolog.Nop()), logger: zerolog.Nop()}
	entry := &storage.Entry{InfoHash: "0123456789ABCDEF0123456789ABCDEF01234567", Name: "converting"}
	m.queue.waits.observe(entry.InfoHash, waitConverting)

	m.cleanupChild(t.Context(), entryChildCall{kind: grab.ChildCleanup, grab: grab.Grab{Entry: entry}})
	if got := m.queue.waits.observe(entry.InfoHash, waitConverting); got != waitNone {
		t.Fatalf("after the cleanup the entry's wait is still %s, want it forgotten", got)
	}
}
