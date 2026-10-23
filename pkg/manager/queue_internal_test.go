package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
			if deleteErr := q.DeleteStalled(noJob); deleteErr != nil {
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
		{"torrent failed past setting", config.ProtocolTorrent, time.Hour, 0, false},
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
			if deleteErr := q.DeleteStalled(noJob); deleteErr != nil {
				t.Fatal(deleteErr)
			}
			if _, getErr := q.GetTorrent(entry.InfoHash); (getErr == nil) != tc.wantKept {
				t.Fatalf("kept = %v, want %v", getErr == nil, tc.wantKept)
			}
		})
	}
}

// The sweep dropped failed NZB entries without their staged file, leaking one .queued file per failure.
func TestDeleteStalledRemovesStagedNZB(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	staged := filepath.Join(t.TempDir(), "job.queued")
	if writeErr := os.WriteFile(staged, []byte("<nzb/>"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	failedAt := time.Now().Add(-25 * time.Hour)
	q := newQueue(store, "10m", nil, zerolog.Nop())
	entry := &storage.Entry{
		InfoHash: "job", Name: "failed", Protocol: config.ProtocolNZB, Magnet: staged,
		AddedOn: failedAt, State: storage.EntryStateError, LastErrorTime: &failedAt,
	}
	if addErr := q.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	action := runningAction(t, q, entry.InfoHash)
	if deleteErr := q.DeleteStalled(noJob); deleteErr != nil {
		t.Fatal(deleteErr)
	}
	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatalf("staged NZB still on disk after the sweep: %v", statErr)
	}
	if !errors.Is(context.Cause(action), errEntryDeleted) {
		t.Fatalf("post-download action cause = %v, want errEntryDeleted", context.Cause(action))
	}
}

// Deleting an entry cancels its post-download action, so the action cannot write the entry back.
func TestQueueDeleteStopsRunningAction(t *testing.T) {
	t.Parallel()
	queue, entry, _ := newQueueDeleteTest(t)
	action := runningAction(t, queue, strings.ToUpper(entry.InfoHash))
	if err := queue.Delete(entry.InfoHash, false, nil); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(action), errEntryDeleted) {
		t.Fatalf("post-download action cause = %v, want errEntryDeleted", context.Cause(action))
	}
}

func runningAction(t *testing.T, q *Queue, infohash string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	t.Cleanup(func() { cancel(nil) })
	q.running.Store(strings.ToLower(infohash), cancel)
	return ctx
}

// A queued entry whose job is gone never leaves the queue; one whose job still waits must stay.
func TestDeleteStalledRemovesQueuedEntriesWithNoJob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		added    time.Duration
		pending  bool
		wantKept bool
	}{
		{"no job past the timeout", 2 * time.Hour, false, false},
		{"job still pending", 2 * time.Hour, true, true},
		{"no job inside the timeout", time.Minute, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			q := newQueue(store, "1h", nil, zerolog.Nop())
			entry := &storage.Entry{
				InfoHash: "0123456789012345678901234567890123456789", Name: "queued",
				AddedOn: time.Now().Add(-tc.added), State: storage.EntryStateDownloading,
				Status: debridTypes.TorrentStatusQueued,
			}
			if addErr := q.Add(entry); addErr != nil {
				t.Fatal(addErr)
			}
			if deleteErr := q.DeleteStalled(func(string) bool { return tc.pending }); deleteErr != nil {
				t.Fatal(deleteErr)
			}
			if _, getErr := q.GetTorrent(entry.InfoHash); (getErr == nil) != tc.wantKept {
				t.Fatalf("kept = %v, want %v", getErr == nil, tc.wantKept)
			}
		})
	}
}

func noJob(string) bool { return false }
