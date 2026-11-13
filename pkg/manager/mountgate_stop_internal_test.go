package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// checkLeftResumable asserts a stop left the entry queued and unfailed.
func checkLeftResumable(t *testing.T, m *Manager, hash string) *storage.Entry {
	t.Helper()
	saved, err := m.queue.GetTorrent(hash)
	if err != nil {
		t.Fatalf("entry %s left the queue: %v", hash, err)
	}
	if saved.State != storage.EntryStateDownloading || saved.LastError != "" || saved.IsComplete {
		t.Fatalf("entry %s is not resumable: state %v, error %q, complete %v",
			hash, saved.State, saved.LastError, saved.IsComplete)
	}
	return saved
}

// newMountStopManager is a hand-built manager with an open mount gate and a one-release torrent whose file never
// appears on the mount.
func newMountStopManager(t *testing.T) (*Manager, *storage.Entry) {
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
	return m, entry
}

// Test 1: a wait whose 2m deadline falls while the mount stops used to fail the grab.
func TestSymlinkWaitDeadlineDuringMountStopIsResumable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, entry := newMountStopManager(t)
		m.downloader.mountWaitTimeout = 2 * time.Minute

		result := runAction(t, m, entry)
		synctest.Wait()
		saved, err := m.queue.GetTorrent(entry.InfoHash)
		if err != nil || saved.State != storage.EntryStateDownloading {
			t.Fatalf("entry before the stop = %#v, %v", saved, err)
		}

		time.Sleep(time.Minute + 59*time.Second)
		m.mountGate.markStopping()
		time.Sleep(time.Minute) // past the deadline
		synctest.Wait()

		assertCutShort(t, result)
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

// A wait already sleeping ends when the gate closes, not at its next attempt.
func TestSleepEndsWhenGateCloses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := symlinkTestDownloader(t)
		d.manager.mountGate = NewMountGate(zerolog.Nop())
		begin := time.Now()
		done := make(chan error, 1)
		go func() {
			done <- d.sleepUntilNextSymlinkAttempt(t.Context(), time.Hour, time.Now().Add(2*time.Hour))
		}()
		time.Sleep(time.Second)
		d.manager.mountGate.markStopping()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, ErrMountStopping) {
				t.Fatalf("err = %v, want ErrMountStopping", err)
			}
		default:
			t.Fatal("the wait did not end when the gate closed")
		}
		if got := time.Since(begin); got != time.Second {
			t.Fatalf("wait ended at +%v, want +1s", got)
		}
	})
}

// Test 2: an add the HTTP side accepted while the mount stops stays queued.
func TestAddDuringMountStopStaysQueued(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, entry := newMountStopManager(t)
		m.mountGate.markStopping()

		result := runAction(t, m, entry)
		synctest.Wait()

		assertCutShort(t, result)
		checkLeftResumable(t, m, entry.InfoHash)
		if _, err := os.Lstat(entry.DownloadPath("")); !os.IsNotExist(err) {
			t.Fatalf("a download folder was made for a stopping mount: %v", err)
		}
	})
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

// runAction starts entry's action child and returns what it reports; a nil event is the stop that cut it short.
func runAction(t *testing.T, m *Manager, entry *storage.Entry) <-chan entryMsg {
	t.Helper()
	if m.entries == nil {
		m.entries = stoppedManager(t).entries
	}
	call := entryChildCall{kind: grab.ChildAction, grab: grab.Grab{Entry: entry}}
	done := make(chan entryMsg, 1)
	go func() { done <- m.actionChild(m.Context(), call) }()
	return done
}

func assertCutShort(t *testing.T, result <-chan entryMsg) {
	t.Helper()
	synctest.Wait()
	select {
	case msg := <-result:
		if msg.ev != nil {
			t.Fatalf("action reported %+v, want nothing so the actor runs it again", msg.ev)
		}
	default:
		t.Fatal("the action did not end")
	}
}
