package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestQueueDeleteKeepsFilesWhenNotRequested(t *testing.T) {
	t.Parallel()
	queue, entry, downloadedPath := newQueueDeleteTest(t)

	if err := queue.Delete(entry.InfoHash, false, nil); err != nil {
		t.Fatalf("delete queued entry: %v", err)
	}
	if _, err := os.Stat(downloadedPath); err != nil {
		t.Fatalf("expected downloaded files to remain: %v", err)
	}
	if _, err := queue.GetTorrent(entry.InfoHash); err == nil {
		t.Fatal("expected queued entry to be removed")
	}
}

func TestQueueDeleteRemovesFilesWhenRequested(t *testing.T) {
	t.Parallel()
	queue, entry, downloadedPath := newQueueDeleteTest(t)

	if err := queue.Delete(entry.InfoHash, true, nil); err != nil {
		t.Fatalf("delete queued entry: %v", err)
	}
	if _, err := os.Stat(downloadedPath); !os.IsNotExist(err) {
		t.Fatalf("expected downloaded files to be removed, got %v", err)
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

	queue := newQueue(store, "", nil, zerolog.Nop())
	if addErr := queue.Add(entry); addErr != nil {
		t.Fatalf("add queued entry: %v", addErr)
	}
	return queue, entry, downloadedPath
}

// An unparsable or non-positive remove_stalled_after disables stalled
// removal; it used to leave a zero cutoff that removed every stalled entry.
func TestDeleteStalledNeedsAValidDuration(t *testing.T) {
	t.Parallel()
	for setting, wantKept := range map[string]bool{"1 day": true, "0s": true, "-1h": true, "1h": false} {
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			q := newQueue(store, setting, nil, zerolog.Nop())
			stalled := &storage.Entry{
				InfoHash: "0123456789012345678901234567890123456789", Name: "stalled",
				AddedOn: time.Now().Add(-48 * time.Hour), Status: debridTypes.TorrentStatusError,
			}
			if addErr := q.Add(stalled); addErr != nil {
				t.Fatal(addErr)
			}
			if deleteErr := q.DeleteStalled(); deleteErr != nil {
				t.Fatal(deleteErr)
			}
			if _, getErr := q.GetTorrent(stalled.InfoHash); (getErr == nil) != wantKept {
				t.Fatalf("remove_stalled_after %q: kept = %v, want %v", setting, getErr == nil, wantKept)
			}
		})
	}
}
