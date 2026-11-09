package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
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

func (c completedTorrentProvider) CheckStatus(context.Context, *types.Torrent) (*types.Torrent, error) {
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
		ID: "provider-id", Debrid: "provider", InfoHash: entry.InfoHash, Name: entry.Name,
		Status: types.TorrentStatusDownloaded, Progress: 100,
		Files: map[string]types.File{"Show.S02E01.mkv": {Name: "Show.S02E01.mkv", ID: "2", Size: 5}},
	}
	if multiSeason {
		torrent.Files["Show.S01E01.mkv"] = types.File{Name: "Show.S01E01.mkv", ID: "1", Size: 5}
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
	if multiSeason && !m.queue.Contains(seasonHolding(t, m, entry, "Show.S02E01.mkv").InfoHash) {
		t.Fatal("shutdown removed the in-flight season")
	}
	for name := range torrent.Files {
		if writeFileErr := os.WriteFile(filepath.Join(mountPath, name), []byte("media"), 0o644); writeFileErr != nil {
			t.Fatal(writeFileErr)
		}
	}
	m.restoreActiveDownloadJobs(m.restoreSnapshot())
	m.processQueuedEntries()
	synctest.Wait()
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || !saved.IsComplete || saved.IsDownloading {
		t.Fatalf("resumed entry = %#v, error = %v", saved, err)
	}
	downloadPath := saved.DownloadPath("")
	if multiSeason {
		downloadPath = seasonDownloadPath(t, m, entry, "Show.S02E01.mkv")
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
	m.restoreActiveDownloadJobs(m.restoreSnapshot())
	m.processQueuedEntries()
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
	m := &Manager{
		storage: store,
		config:  cfg,
		store:   st,
		queue: newQueue(
			store,
			"",
			nil,
			zerolog.Nop(),
		),
		arr:               arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop()),
		logger:            zerolog.Nop(),
		clients:           xsync.NewMap[string, debrid.Client](),
		processingEntries: xsync.NewMap[string, struct{}](),
	}
	wireLifecycle(t, m)
	m.initEntryCache()
	m.downloader = NewDownloadManager(m)
	return m
}

// wireLifecycle gives a hand-built manager the task tree New would: a group
// of its own, the supervised sub-services, a notification service on the new
// group and the storage-closing task. A stopped manager reopened on the same
// database calls it again.
func wireLifecycle(t *testing.T, m *Manager) {
	t.Helper()
	m.group = testGroup(t)
	m.life = newLifecycle()
	m.initGrace(m.group.Context())
	m.ready = make(chan struct{})
	m.startDone = make(chan struct{})
	m.startRan = false
	m.children = nil
	if err := m.registerServices(); err != nil {
		t.Fatal(err)
	}
	notifyCfg := &config.Notifications{}
	if m.config != nil {
		notifyCfg = &m.config.Notifications
	}
	m.Notifications = notifications.New(m.notifyGroup, notifyCfg, nil, zerolog.Nop())
	if err := m.group.Go("storage", m.closeStorage); err != nil {
		t.Fatal(err)
	}
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
// mount; the stop closes the mount gate, so the entry is saved resumable.
func interruptAndStop(t *testing.T, m *Manager, entry *storage.Entry, torrent *types.Torrent) {
	t.Helper()
	m.processNewTorrent(entry, torrent)
	synctest.Wait()
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || !saved.IsDownloading || saved.IsComplete {
		t.Fatalf("in-flight entry = %#v, error = %v", saved, err)
	}
	if stopErr := m.Stop(t.Context()); stopErr != nil {
		t.Fatal(stopErr)
	}
	if lateErr := m.runWork(
		"late",
		func() { t.Error("work started after shutdown") },
	); !errors.Is(
		lateErr,
		task.ErrStopped,
	) {
		t.Fatalf("work after shutdown: err = %v, want ErrStopped", lateErr)
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
	wireLifecycle(t, m)
	m.jobQueue = NewJobQueue(m.work, 1, m.processJob, zerolog.Nop())
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
func seasonDownloadPath(t *testing.T, m *Manager, entry *storage.Entry, file string) string {
	t.Helper()
	return seasonHolding(t, m, entry, file).DownloadPath("")
}

// seasonHolding returns the season entry of the pack that holds file.
func seasonHolding(t *testing.T, m *Manager, entry *storage.Entry, file string) *storage.Entry {
	t.Helper()
	_, seasons := m.downloader.detectMultiSeason(entry)
	for _, season := range convertToMultiSeason(entry, seasons) {
		if season.Files[file] != nil {
			return season
		}
	}
	t.Fatalf("no season holds %s", file)
	return nil
}
