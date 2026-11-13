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
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/notifications"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type completedTorrentProvider struct {
	debrid.Client

	torrent *types.Torrent
}

func (c completedTorrentProvider) CheckStatus(context.Context, *types.Torrent) (*types.Torrent, error) {
	return c.torrent, nil
}

func (c completedTorrentProvider) Poll(
	context.Context,
	*types.Torrent,
	bool,
) (*types.Torrent, types.Observation, error) {
	if c.torrent == nil {
		return nil, types.Observation{}, nil
	}
	return c.torrent, observationOfStatus(c.torrent), nil
}

func TestShutdownResumesInterruptedSymlinks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, runShutdownResume)
}

func runShutdownResume(t *testing.T) {
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
		SkipMultiSeason: true,
		SavePath:        t.TempDir(),
		Files:           map[string]*storage.File{},
		Providers:       map[string]*storage.ProviderEntry{},
	}
	torrent := &types.Torrent{
		ID: "provider-id", Debrid: "provider", InfoHash: entry.InfoHash, Name: entry.Name,
		Status: types.TorrentStatusDownloaded, Progress: 100,
		Files: map[string]types.File{"Show.S02E01.mkv": {Name: "Show.S02E01.mkv", ID: "2", Size: 5}},
	}
	applyDebridTorrentToEntry(entry, torrent)
	m.clients.Store("provider", completedTorrentProvider{torrent: torrent})
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	mountPath := m.GetTorrentMountPath(entry)
	if mkdirAllErr := os.MkdirAll(mountPath, 0o755); mkdirAllErr != nil {
		t.Fatal(mkdirAllErr)
	}

	interruptAndStop(t, m, entry)
	reopenShutdownTestManager(t, m, dbPath, entry)
	for name := range torrent.Files {
		if writeFileErr := os.WriteFile(filepath.Join(mountPath, name), []byte("media"), 0o644); writeFileErr != nil {
			t.Fatal(writeFileErr)
		}
	}
	resumed := runAction(t, m, entry)
	synctest.Wait()
	msg := <-resumed
	if msg.ev == nil || msg.ev.Kind != grab.EvActionDone {
		t.Fatalf("resumed action reported %+v, want ActionDone", msg.ev)
	}
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || msg.update == nil || saved.IsDownloading {
		t.Fatalf("resumed entry = %#v, error = %v", saved, err)
	}
	downloadPath := saved.DownloadPath("")
	linkPath := filepath.Join(downloadPath, "Show.S02E01.mkv")
	target, err := os.Readlink(linkPath)
	if err != nil || target != filepath.Join(mountPath, "Show.S02E01.mkv") {
		t.Fatalf("restored symlink = %q, error = %v", target, err)
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
		grabs:   &grabIndex{},
		config:  cfg,
		store:   st,
		queue: newQueue(store, nil,
			zerolog.Nop(),
		),
		arr:     arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop()),
		logger:  zerolog.Nop(),
		clients: xsync.NewMap[string, debrid.Client](),
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

// interruptAndStop starts the import, then shuts down while it waits for the
// mount; the stop closes the mount gate and the action ends with no result, so the grab runs it again.
func interruptAndStop(t *testing.T, m *Manager, entry *storage.Entry) {
	t.Helper()
	result := runAction(t, m, entry)
	synctest.Wait()
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil || saved.IsComplete {
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
	assertCutShort(t, result)
}

// reopenShutdownTestManager simulates a restart on the same database and
// checks the interrupted entry is resumable.
func reopenShutdownTestManager(t *testing.T, m *Manager, dbPath string, entry *storage.Entry) {
	t.Helper()
	store, err := storage.NewStorage(dbPath, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m.storage, m.queue = store, newQueue(store, nil, zerolog.Nop())
	wireLifecycle(t, m)
	saved, err := m.queue.GetTorrent(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != storage.EntryStateDownloading || saved.IsComplete ||
		saved.LastError != "" {
		t.Fatalf("interrupted entry cannot resume: %#v", saved)
	}
}
