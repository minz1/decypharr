package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// checkLeftResumable asserts a stop left the entry queued, idle and unfailed.
func checkLeftResumable(t *testing.T, m *Manager, hash string) *storage.Entry {
	t.Helper()
	saved, err := m.queue.GetTorrent(hash)
	if err != nil {
		t.Fatalf("entry %s left the queue: %v", hash, err)
	}
	if saved.State != storage.EntryStateDownloading || saved.LastError != "" || saved.IsComplete ||
		saved.IsDownloading {
		t.Fatalf("entry %s is not resumable: state %v, error %q, complete %v, downloading %v",
			hash, saved.State, saved.LastError, saved.IsComplete, saved.IsDownloading)
	}
	return saved
}

// newMountStopManager is a hand-built manager with an open mount gate and a one-release torrent whose file never
// appears on the mount.
func newMountStopManager(t *testing.T) (*Manager, *storage.Entry, *types.Torrent) {
	t.Helper()
	m := newShutdownTestManager(t, t.TempDir())
	m.mountGate = NewMountGate(zerolog.Nop())
	entry := &storage.Entry{
		InfoHash:        "0123456789012345678901234567890123456789",
		Name:            "Show.S01E01",
		Protocol:        config.ProtocolTorrent,
		State:           storage.EntryStateDownloading,
		ActiveProvider:  "provider",
		Action:          config.DownloadActionSymlink,
		SkipMultiSeason: true,
		SavePath:        t.TempDir(),
		Files:           map[string]*storage.File{},
		Providers:       map[string]*storage.ProviderEntry{},
	}
	torrent := &types.Torrent{
		ID: "provider-id", Debrid: "provider", InfoHash: entry.InfoHash, Name: entry.Name,
		Status: types.TorrentStatusDownloaded, Progress: 100,
		Files: map[string]types.File{"Show.S01E01.mkv": {Name: "Show.S01E01.mkv", ID: "1", Size: 5}},
	}
	applyDebridTorrentToEntry(entry, torrent)
	m.clients.Store("provider", completedTorrentProvider{torrent: torrent})
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.GetTorrentMountPath(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	return m, entry, torrent
}

// Test 1: a wait whose 2m deadline falls while the mount stops used to fail the grab.
func TestSymlinkWaitDeadlineDuringMountStopIsResumable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, entry, torrent := newMountStopManager(t)
		m.downloader.mountWaitTimeout = 2 * time.Minute

		m.processNewTorrent(entry, torrent)
		synctest.Wait()
		saved, err := m.queue.GetTorrent(entry.InfoHash)
		if err != nil || saved.State != storage.EntryStateDownloading {
			t.Fatalf("entry before the stop = %#v, %v", saved, err)
		}

		time.Sleep(time.Minute + 59*time.Second)
		m.mountGate.markStopping()
		time.Sleep(time.Minute) // past the deadline
		synctest.Wait()

		checkLeftResumable(t, m, entry.InfoHash)
	})
}

func TestCreateSymlinksWaitEndsWithMountStopping(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := symlinkTestDownloader(t)
		d.manager.mountGate = NewMountGate(zerolog.Nop())
		d.mountWaitTimeout = 2 * time.Minute
		done := make(chan error, 1)
		go func() {
			_, err := d.createSymlinksWhenMountFilesAppear(
				t.Context(),
				&storage.Entry{Name: "release"},
				[]*storage.File{{Name: "release.mkv"}},
				t.TempDir(),
				t.TempDir(),
			)
			done <- err
		}()
		synctest.Wait()
		time.Sleep(time.Minute + 59*time.Second)
		d.manager.mountGate.markStopping()
		time.Sleep(time.Minute)
		synctest.Wait()
		if err := <-done; !errors.Is(err, ErrMountStopping) {
			t.Fatalf("err = %v, want ErrMountStopping", err)
		}
	})
}

func TestSleepReturnsMountStoppingWhenDeadlinePassed(t *testing.T) {
	t.Parallel()
	d := symlinkTestDownloader(t)
	d.manager.mountGate = NewMountGate(zerolog.Nop())
	past := time.Now().Add(-time.Second)
	if err := d.sleepUntilNextSymlinkAttempt(t.Context(), time.Second, past); err != nil {
		t.Fatalf("open gate, passed deadline: err = %v, want nil", err)
	}
	d.manager.mountGate.markStopping()
	if err := d.sleepUntilNextSymlinkAttempt(t.Context(), time.Second, past); !errors.Is(err, ErrMountStopping) {
		t.Fatalf("closed gate, passed deadline: err = %v, want ErrMountStopping", err)
	}
	if err := d.sleepUntilNextSymlinkAttempt(t.Context(), time.Second, time.Now().Add(time.Hour)); !errors.Is(
		err, ErrMountStopping) {
		t.Fatalf("closed gate: err = %v, want ErrMountStopping", err)
	}
}

// Test 2: an add the HTTP side accepted while the mount stops stays queued.
func TestAddDuringMountStopStaysQueued(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, entry, torrent := newMountStopManager(t)
		m.mountGate.markStopping()

		m.processNewTorrent(entry, torrent)
		synctest.Wait()

		checkLeftResumable(t, m, entry.InfoHash)
		if _, err := os.Lstat(entry.DownloadPath("")); !os.IsNotExist(err) {
			t.Fatalf("a download folder was made for a stopping mount: %v", err)
		}
	})
}

// Test 6: a pack whose second season meets the closing gate is not complete, and its linked season stays done.
func TestSeasonPackMountStopKeepsParentQueued(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, entry, torrent, children := newSeasonPackTest(t, "Show.S01-S02")
		m.mountGate = NewMountGate(zerolog.Nop())
		m.downloader.mountWaitTimeout = 2 * time.Minute
		mountPath := m.GetTorrentMountPath(entry)
		if err := os.WriteFile(filepath.Join(mountPath, "Show.S01E01.mkv"), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}

		m.processNewTorrent(entry, torrent)
		synctest.Wait()
		m.mountGate.markStopping()
		synctest.Wait()

		for _, child := range children {
			checkSeasonAfterMountStop(t, m, child, child.Files["Show.S01E01.mkv"] != nil)
		}
		checkLeftResumable(t, m, entry.InfoHash)
	})
}

// checkSeasonAfterMountStop checks season 1 stayed complete and season 2, which was waiting for its mount file
// (IsDownloading is set only after that wait), stayed queued and idle; neither failed.
func checkSeasonAfterMountStop(t *testing.T, m *Manager, season *storage.Entry, first bool) {
	t.Helper()
	saved, err := m.queue.GetTorrent(season.InfoHash)
	if err != nil {
		t.Fatalf("season %s left the queue: %v", season.Name, err)
	}
	if saved.State == storage.EntryStateError || saved.LastError != "" {
		t.Fatalf("season %s failed: %#v", season.Name, saved)
	}
	if saved.IsComplete != first || saved.IsDownloading {
		t.Errorf("season %s: complete %v, downloading %v; want complete %v, idle",
			season.Name, saved.IsComplete, saved.IsDownloading, first)
	}
}

// A delete that races the mount stop still removes the season it cut short.
func TestSeasonPackDeleteDuringMountStopRemovesSeason(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, entry, torrent, children := newSeasonPackTest(t, "Show.S01-S02")
		m.mountGate = NewMountGate(zerolog.Nop())
		m.downloader.mountWaitTimeout = 2 * time.Minute

		m.processNewTorrent(entry, torrent)
		synctest.Wait()
		m.mountGate.markStopping()
		if err := m.queue.Delete(entry.InfoHash, true, nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		for _, child := range children {
			if m.queue.Contains(child.InfoHash) {
				t.Errorf("season %s still queued after its pack was deleted", child.Name)
			}
		}
	})
}

// statusCounter is a provider that counts status checks.
type statusCounter struct {
	debrid.Client

	checks *atomic.Int32
}

func (c statusCounter) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	c.checks.Add(1)
	return torrent, nil
}

// Test 9: an entry saved resumable is not re-driven while the mount stops; a Download-action entry still is.
func TestPollSkipsSymlinkEntriesWhileMountStops(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		action config.DownloadAction
		driven bool
	}{
		{"symlink", config.DownloadActionSymlink, false},
		{"unknown action", config.DownloadAction("other"), false},
		{"download", config.DownloadActionDownload, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				m, entry, _ := newMountStopManager(t)
				checks := &atomic.Int32{}
				m.clients.Store("provider", statusCounter{checks: checks})
				entry.Action = tc.action
				entry.Status = types.TorrentStatusDownloaded
				entry.IsDownloading = false
				if err := m.queue.Update(entry); err != nil {
					t.Fatal(err)
				}
				m.mountGate.markStopping()

				m.pollEntry(entry)
				synctest.Wait()

				if got := checks.Load() > 0; got != tc.driven {
					t.Fatalf("status checked = %v, want %v", got, tc.driven)
				}
				if _, held := m.processingEntries.Load(entry.InfoHash); held {
					t.Fatal("the poll left its claim behind")
				}
			})
		})
	}
}

func TestWarmSkippedWhileMountStops(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "release.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, stopping := range []bool{false, true} {
		var logs bytes.Buffer
		d := symlinkTestDownloader(t)
		d.logger = zerolog.New(&logs).Level(zerolog.DebugLevel)
		d.manager.logger = zerolog.Nop()
		d.manager.mountGate = NewMountGate(zerolog.Nop())
		if stopping {
			d.manager.mountGate.markStopping()
		}

		d.warmAfterLinks(context.Background(), &storage.Entry{Name: "release"}, []string{path})

		if warmed := strings.Contains(logs.String(), "Warming cache for"); warmed == stopping {
			t.Fatalf("stopping %v: warm ran = %v; logs %q", stopping, warmed, logs.String())
		}
	}
}
