package manager

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
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
		ID: id, Debrid: "provider", InfoHash: "0123456789012345678901234567890123456789",
		Files: map[string]types.File{"video.mkv": {Name: "video.mkv"}},
	}, nil
}

func (incompleteProvider) UpdateTorrent(*types.Torrent) error { return nil }

// The link service dereferences the refresher's entry; an incomplete provider
// torrent must hand back the stored entry, never (nil, nil).
func TestRefreshTorrentIncompleteReturnsStoredEntry(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(
		t,
		&Manager{storage: store, clients: xsync.NewMap[string, debrid.Client](), logger: zerolog.Nop()},
	)
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
func TestResumeJobSkipsEntryAlreadyInFlight(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client := &countingStatusProvider{}
	m := withTestConfig(t, &Manager{
		queue: newQueue(
			store,
			"",
			nil,
			zerolog.Nop(),
		),
		clients:           xsync.NewMap[string, debrid.Client](),
		logger:            zerolog.Nop(),
		processingEntries: xsync.NewMap[string, struct{}](),
	})
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

// failingStatusProvider fails every status check with err and records
// deletions.
type failingStatusProvider struct {
	debrid.Client

	err     error
	deleted atomic.Int64
}

func (f *failingStatusProvider) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	return torrent, f.err
}

func (f *failingStatusProvider) DeleteTorrent(string) error {
	f.deleted.Add(1)
	return nil
}

// A transient status-check failure must not fail the import or delete the
// provider's torrent; a definitive one still does.
func TestQueuedTorrentSurvivesTransientStatusErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		err       error
		wantError bool
	}{
		{name: "timeout", err: context.DeadlineExceeded, wantError: false},
		{name: "connection reset", err: syscall.ECONNRESET, wantError: false},
		{name: "provider unavailable", err: customerror.ErrHosterUnavailable, wantError: false},
		{name: "not cached", err: customerror.ErrTorrentNotCached, wantError: true},
		{name: "blocked", err: customerror.ErrTorrentBlocked, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := storage.NewStorage(t.TempDir(), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			provider := &failingStatusProvider{err: tc.err}
			m := withTestConfig(t, &Manager{
				queue: newQueue(store, "", nil, zerolog.Nop()), clients: xsync.NewMap[string, debrid.Client](),
				logger: zerolog.Nop(), processingEntries: xsync.NewMap[string, struct{}](),
			})
			m.clients.Store("provider", provider)
			entry := &storage.Entry{
				InfoHash: "0123456789012345678901234567890123456789", Name: "release",
				Protocol: config.ProtocolTorrent, ActiveProvider: "provider", State: storage.EntryStateDownloading,
				Providers: map[string]*storage.ProviderEntry{"provider": {Provider: "provider", ID: "id"}},
			}
			if addErr := m.queue.Add(entry); addErr != nil {
				t.Fatal(addErr)
			}
			m.processQueuedTorrent(entry)
			saved, err := m.queue.GetTorrent(entry.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			if failed := saved.State == storage.EntryStateError; failed != tc.wantError {
				t.Fatalf("state = %q, want failed=%v", saved.State, tc.wantError)
			}
			if !tc.wantError && provider.deleted.Load() != 0 {
				t.Fatal("a transient failure deleted the provider's torrent")
			}
		})
	}
}
