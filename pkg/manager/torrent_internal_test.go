package manager

import (
	"sync/atomic"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// incompleteProvider reports a torrent whose files never gain download links.
type incompleteProvider struct {
	debrid.Client
}

func (incompleteProvider) GetTorrent(id string) (*types.Torrent, error) {
	return &types.Torrent{
		Id: id, Debrid: "provider", InfoHash: "0123456789012345678901234567890123456789",
		Files: map[string]types.File{"video.mkv": {Name: "video.mkv"}},
	}, nil
}

func (incompleteProvider) UpdateTorrent(*types.Torrent) error { return nil }

// The link service dereferences the refresher's entry; an incomplete provider
// torrent must hand back the stored entry, never (nil, nil).
func TestRefreshTorrentIncompleteReturnsStoredEntry(t *testing.T) { //nolint:paralleltest // mutates the config singleton
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := &Manager{storage: store, clients: xsync.NewMap[string, debrid.Client](), logger: zerolog.Nop()}
	m.clients.Store("provider", incompleteProvider{})
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release",
		Protocol: config.ProtocolTorrent, ActiveProvider: "provider",
		Files: map[string]*storage.File{"video.mkv": {Name: "video.mkv", Size: 1}},
		Providers: map[string]*storage.ProviderEntry{
			"provider": {Provider: "provider", ID: "id", Status: types.TorrentStatusDownloaded},
		},
	}
	if addErr := store.AddOrUpdate(entry); addErr != nil {
		t.Fatal(addErr)
	}

	refreshed, err := m.refreshTorrent(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed == nil || refreshed.InfoHash != entry.InfoHash {
		t.Fatalf("refreshed entry = %#v, want the stored entry", refreshed)
	}
}

type countingStatusProvider struct {
	debrid.Client

	checks atomic.Int64
}

func (c *countingStatusProvider) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	c.checks.Add(1)
	return torrent, nil
}

// A resumed job must not drive an entry the queue scheduler is already
// processing, nor release the scheduler's in-flight claim.
func TestResumeJobSkipsEntryAlreadyInFlight(t *testing.T) { //nolint:paralleltest // mutates the config singleton
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client := &countingStatusProvider{}
	m := &Manager{
		queue: newQueue(store, ""), clients: xsync.NewMap[string, debrid.Client](), logger: zerolog.Nop(),
		processingEntries: xsync.NewMap[string, struct{}](),
	}
	m.clients.Store("provider", client)
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release",
		Protocol: config.ProtocolTorrent, State: storage.EntryStateDownloading,
		Status: types.TorrentStatusDownloading, ActiveProvider: "provider",
		Providers: map[string]*storage.ProviderEntry{"provider": {Provider: "provider", ID: "id"}},
	}
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	m.processingEntries.Store(entry.InfoHash, struct{}{})

	job := &Job{ID: entry.InfoHash, Type: JobTypeTorrent, Entry: entry, ResumeExisting: true}
	if jobErr := m.processTorrentJob(t.Context(), job); jobErr != nil {
		t.Fatal(jobErr)
	}
	if got := client.checks.Load(); got != 0 {
		t.Fatalf("provider status checks = %d, want 0", got)
	}
	if _, ok := m.processingEntries.Load(entry.InfoHash); !ok {
		t.Fatal("resumed job released the scheduler's in-flight claim")
	}
}
