package manager

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
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

	queue := newQueue(store, "", nil, zerolog.Nop())
	if addErr := queue.Add(entry); addErr != nil {
		t.Fatalf("add queued entry: %v", addErr)
	}
	return queue, entry, downloadedPath
}

// An unparsable or non-positive remove_stalled_after disables stalled
// removal; it used to leave a zero cutoff that removed every stalled entry.
func TestStalledNeedsAValidDuration(t *testing.T) {
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
			kept := !sweptOut(t, q, stalled.InfoHash, noJob)
			if kept != wantKept {
				t.Fatalf("remove_stalled_after %q: kept = %v, want %v", setting, kept, wantKept)
			}
		})
	}
}

// A failed entry must stay visible long enough for the arr to read it from history;
// timing the sweep from the add hid late failures, and the arr never re-searched.
func TestStalledTimesFailuresFromTheFailure(t *testing.T) {
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
			kept := !sweptOut(t, q, entry.InfoHash, noJob)
			if kept != tc.wantKept {
				t.Fatalf("kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

// The sweep dropped failed NZB entries without their staged file, leaking one .queued file per failure.
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

// A queued entry whose job is gone never leaves the queue; one whose job still waits must stay.
func TestStalledRemovesQueuedEntriesWithNoJob(t *testing.T) {
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
			kept := !sweptOut(t, q, entry.InfoHash, func(string) bool { return tc.pending })
			if kept != tc.wantKept {
				t.Fatalf("kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

func noJob(string) bool { return false }

func sweptOut(t *testing.T, q *Queue, hash string, pending func(string) bool) bool {
	t.Helper()
	stalled, err := q.Stalled(pending)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(stalled, func(e *storage.Entry) bool { return e.InfoHash == hash })
}

// An entry whose grab is cleaned up forgets its provider wait: the map stays bounded, and a later grab of the same
// hash logs its wait's start at WARN again.
func TestCleanupForgetsTheProviderWait(t *testing.T) {
	t.Parallel()
	m := &Manager{queue: newQueue(nil, "1h", nil, zerolog.Nop()), logger: zerolog.Nop()}
	entry := &storage.Entry{InfoHash: "0123456789ABCDEF0123456789ABCDEF01234567", Name: "converting"}
	m.queue.waits.observe(entry.InfoHash, waitConverting)

	m.cleanupChild(t.Context(), entryChildCall{kind: grab.ChildCleanup, grab: grab.Grab{Entry: entry}})
	if got := m.queue.waits.observe(entry.InfoHash, waitConverting); got != waitNone {
		t.Fatalf("after the cleanup the entry's wait is still %s, want it forgotten", got)
	}
}

// A local download resets Progress to 0 when it starts; the sweep used to take it for a stalled entry and cancel it.
func TestStalledKeepsARunningLocalDownload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		downloading bool
		wantKept    bool
	}{
		{"download just started", true, true},
		{"nothing running", false, false},
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
				InfoHash: "0123456789012345678901234567890123456789", Name: "downloading",
				AddedOn: time.Now().Add(-2 * time.Hour), State: storage.EntryStateDownloading,
				Status: debridTypes.TorrentStatusDownloaded, IsDownloading: tc.downloading,
			}
			if addErr := q.Add(entry); addErr != nil {
				t.Fatal(addErr)
			}
			kept := !sweptOut(t, q, entry.InfoHash, noJob)
			if kept != tc.wantKept {
				t.Fatalf("kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}
