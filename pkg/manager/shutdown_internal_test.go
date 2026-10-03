package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/notifications"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type shutdownMount struct {
	MountManager

	stop func() error
}

func (m shutdownMount) Stop() error            { return m.stop() }
func (m shutdownMount) Refresh([]string) error { return nil }

type completedTorrentProvider struct {
	debrid.Client

	torrent *types.Torrent
}

func (c completedTorrentProvider) CheckStatus(*types.Torrent) (*types.Torrent, error) {
	return c.torrent, nil
}

func TestShutdownResumesInterruptedSymlinks(t *testing.T) {
	t.Parallel()
	for _, multiSeason := range []bool{false, true} {
		t.Run(map[bool]string{false: "single release", true: "season pack"}[multiSeason], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				runShutdownResume(t, multiSeason)
			})
		})
	}
}

func runShutdownResume(t *testing.T, multiSeason bool) {
	t.Helper()
	dbPath := t.TempDir()
	m := newShutdownTestManager(t, dbPath)
	entry := &storage.Entry{
		InfoHash:        "0123456789012345678901234567890123456789",
		Name:            "Show.S01-S02",
		Protocol:        config.ProtocolTorrent,
		State:           storage.EntryStateDownloading,
		ActiveProvider:  "provider",
		Action:          config.DownloadActionSymlink,
		SkipMultiSeason: !multiSeason,
		SavePath:        t.TempDir(),
		Files:           map[string]*storage.File{},
		Providers:       map[string]*storage.ProviderEntry{},
	}
	torrent := &types.Torrent{
		Id: "provider-id", Debrid: "provider", InfoHash: entry.InfoHash, Name: entry.Name,
		Status: types.TorrentStatusDownloaded, Progress: 100,
		Files: map[string]types.File{"Show.S02E01.mkv": {Name: "Show.S02E01.mkv", Id: "2", Size: 5}},
	}
	if multiSeason {
		torrent.Files["Show.S01E01.mkv"] = types.File{Name: "Show.S01E01.mkv", Id: "1", Size: 5}
	}
	applyDebridTorrentToEntry(entry, torrent)
	m.clients.Store("provider", completedTorrentProvider{torrent: torrent})
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	var completedSeason *storage.Entry
	if multiSeason {
		completedSeason = seedCompletedSeason(t, m, entry)
	}
	mountPath := m.GetTorrentMountPath(entry)
	if mkdirAllErr := os.MkdirAll(mountPath, 0o755); mkdirAllErr != nil {
		t.Fatal(mkdirAllErr)
	}

	interruptAndStop(t, m, entry, torrent)
	reopenShutdownTestManager(t, m, dbPath, entry)
	for name := range torrent.Files {
		if writeFileErr := os.WriteFile(filepath.Join(mountPath, name), []byte("media"), 0o644); writeFileErr != nil {
			t.Fatal(writeFileErr)
		}
	}
	m.restoreActiveDownloadJobs()
	m.processQueuedEntries()
	m.downloadTasks.Wait()
	synctest.Wait()
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || !saved.IsComplete || saved.IsDownloading {
		t.Fatalf("resumed entry = %#v, error = %v", saved, err)
	}
	downloadPath := saved.DownloadPath("")
	if multiSeason {
		downloadPath = seasonDownloadPath(m, entry, "Show.S02E01.mkv")
		if _, lstatErr := os.Lstat(filepath.Join(completedSeason.DownloadPath(""), "Show.S01E01.mkv")); !os.IsNotExist(
			lstatErr,
		) {
			t.Fatalf("completed season was processed again: %v", lstatErr)
		}
	}
	linkPath := filepath.Join(downloadPath, "Show.S02E01.mkv")
	target, err := os.Readlink(linkPath)
	if err != nil || target != filepath.Join(mountPath, "Show.S02E01.mkv") {
		t.Fatalf("restored symlink = %q, error = %v", target, err)
	}
	if removeErr := os.Remove(linkPath); removeErr != nil {
		t.Fatal(removeErr)
	}
	m.restoreActiveDownloadJobs()
	m.processQueuedEntries()
	m.downloadTasks.Wait()
	if _, lstatErr := os.Lstat(linkPath); !os.IsNotExist(lstatErr) {
		t.Fatalf("completed import was processed again: %v", lstatErr)
	}
}

func newShutdownTestManager(t *testing.T, dbPath string) *Manager {
	t.Helper()
	st := testConfigStore(t, func(cfg *config.Config) {
		cfg.Mount.MountPath = t.TempDir()
		cfg.SkipPreCache = true
	})
	cfg := st.Get()
	store, err := storage.NewStorage(dbPath, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	m := &Manager{
		storage:         store,
		config:          cfg,
		store:           st,
		ctx:             ctx,
		cancelDownloads: cancel,
		queue: newQueue(
			store,
			"",
			nil,
			zerolog.Nop(),
		),
		arr:               arr.New(config.NewStore(&config.Config{}), zerolog.Nop()),
		logger:            zerolog.Nop(),
		clients:           xsync.NewMap[string, debrid.Client](),
		processingEntries: xsync.NewMap[string, struct{}](),
		Notifications:     notifications.New(&cfg.Notifications, zerolog.Nop()),
	}
	m.initEntryCache()
	m.downloader = NewDownloadManager(m)
	return m
}

// seedCompletedSeason queues season 1 of the pack as already imported.
func seedCompletedSeason(t *testing.T, m *Manager, entry *storage.Entry) *storage.Entry {
	t.Helper()
	found, seasons := m.downloader.detectMultiSeason(entry)
	if !found {
		t.Fatal("season pack was not detected")
	}
	var completed *storage.Entry
	for _, season := range convertToMultiSeason(entry, seasons) {
		if season.Files["Show.S01E01.mkv"] == nil {
			continue
		}
		completed = season
		season.MarkAsCompleted(season.DownloadPath(""))
		if addErr := m.queue.Add(season); addErr != nil {
			t.Fatal(addErr)
		}
	}
	return completed
}

// interruptAndStop starts the import, then shuts down while it waits for the
// mount, checking the mount stops only after the entry was saved resumable.
func interruptAndStop(t *testing.T, m *Manager, entry *storage.Entry, torrent *types.Torrent) {
	t.Helper()
	mountStops := 0
	m.mountManager = shutdownMount{stop: func() error {
		mountStops++
		saved, getTorrentErr := m.queue.GetTorrent(entry.InfoHash)
		if getTorrentErr != nil {
			return getTorrentErr
		}
		if saved.IsDownloading {
			t.Error("mount stopped before interrupted work was saved")
		}
		return nil
	}}
	m.processNewTorrent(entry, torrent)
	synctest.Wait()
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || !saved.IsDownloading || saved.IsComplete {
		t.Fatalf("in-flight entry = %#v, error = %v", saved, err)
	}
	if stopErr := m.Stop(); stopErr != nil {
		t.Fatal(stopErr)
	}
	if mountStops != 1 {
		t.Fatalf("mount stops = %d", mountStops)
	}
	if m.startDownloadTask(func() { t.Error("work started after shutdown") }) {
		t.Fatal("shutdown accepted new work")
	}
	synctest.Wait()
}

// reopenShutdownTestManager simulates a restart on the same database and
// checks the interrupted entry is resumable.
func reopenShutdownTestManager(t *testing.T, m *Manager, dbPath string, entry *storage.Entry) {
	t.Helper()
	store, err := storage.NewStorage(dbPath, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m.storage, m.queue = store, newQueue(store, "", nil, zerolog.Nop())
	m.ctx, m.cancelDownloads = context.WithCancel(t.Context())
	m.downloadsStopped = false
	t.Cleanup(func() { _ = m.Stop() })
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != storage.EntryStateDownloading || saved.IsDownloading || saved.IsComplete ||
		saved.LastError != "" {
		t.Fatalf("interrupted entry cannot resume: %#v", saved)
	}
}

// seasonDownloadPath returns the download path of the season holding file.
func seasonDownloadPath(m *Manager, entry *storage.Entry, file string) string {
	_, seasons := m.downloader.detectMultiSeason(entry)
	for _, season := range convertToMultiSeason(entry, seasons) {
		if season.Files[file] != nil {
			return season.DownloadPath("")
		}
	}
	return ""
}
