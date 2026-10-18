package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
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

// A failed entry must stay visible long enough for the arr to read it from history;
// timing the sweep from the add hid late failures, and the arr never re-searched.
func TestDeleteStalledTimesFailuresFromTheFailure(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name     string
		protocol config.Protocol
		failed   time.Duration
		added    time.Duration
		wantKept bool
	}{
		{"nzb added long ago, failed recently", config.ProtocolNZB, time.Hour, 30 * time.Hour, true},
		{"nzb failed just now", config.ProtocolNZB, time.Minute, 0, true},
		{"nzb failed hours ago, inside floor", config.ProtocolNZB, 5 * time.Hour, 0, true},
		{"nzb failed past floor", config.ProtocolNZB, 25 * time.Hour, 0, false},
		{"torrent failed just now", config.ProtocolTorrent, time.Minute, 0, true},
		{"torrent failed past floor", config.ProtocolTorrent, 25 * time.Hour, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			q := newQueue(store, "10m", nil, zerolog.Nop())
			failedAt := now.Add(-tc.failed)
			entry := &storage.Entry{
				InfoHash: "0123456789012345678901234567890123456789", Name: "failed", Protocol: tc.protocol,
				AddedOn: now.Add(-max(tc.added, tc.failed+11*time.Minute)),
				State:   storage.EntryStateError, Status: debridTypes.TorrentStatusError, LastErrorTime: &failedAt,
			}
			if addErr := q.Add(entry); addErr != nil {
				t.Fatal(addErr)
			}
			if deleteErr := q.DeleteStalled(); deleteErr != nil {
				t.Fatal(deleteErr)
			}
			if _, getErr := q.GetTorrent(entry.InfoHash); (getErr == nil) != tc.wantKept {
				t.Fatalf("kept = %v, want %v", getErr == nil, tc.wantKept)
			}
		})
	}
}
