package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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
	status  types.TorrentStatus
	deleted atomic.Int64
}

func (f *failingStatusProvider) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	if f.status != "" {
		torrent.Status = f.status
	}
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

type removedDuringPollProvider struct {
	debrid.Client

	onCheck func()
	err     error
}

func (p *removedDuringPollProvider) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	p.onCheck()
	torrent.Status = types.TorrentStatusDownloading
	return torrent, p.err
}

func (*removedDuringPollProvider) DeleteTorrent(string) error { return nil }

// An entry the arr removes while its status check is in flight stays removed: no error record, no failed grab.
func TestQueuedTorrentRemovedDuringStatusCheckStaysRemoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"provider torrent already deleted", customerror.ErrTorrentNotFound},
		{"still downloading", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := storage.NewStorage(t.TempDir(), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			m := withTestConfig(t, &Manager{
				queue: newQueue(store, "", nil, zerolog.Nop()), clients: xsync.NewMap[string, debrid.Client](),
				logger: zerolog.Nop(), processingEntries: xsync.NewMap[string, struct{}](),
			})
			entry := &storage.Entry{
				InfoHash: "0123456789012345678901234567890123456789", Name: "release", Manual: true,
				Protocol: config.ProtocolTorrent, ActiveProvider: "provider", State: storage.EntryStateDownloading,
				Providers: map[string]*storage.ProviderEntry{"provider": {Provider: "provider", ID: "id"}},
			}
			m.clients.Store("provider", &removedDuringPollProvider{err: tc.err, onCheck: func() {
				if deleteErr := m.queue.Delete(entry.InfoHash, false, nil); deleteErr != nil {
					t.Error(deleteErr)
				}
			}})
			if addErr := m.queue.Add(entry); addErr != nil {
				t.Fatal(addErr)
			}
			m.processQueuedTorrent(entry)
			if m.queue.Contains(entry.InfoHash) {
				t.Fatal("the removed entry was written back")
			}
		})
	}
}

// deletedDuringPollProvider answers a status check only once its torrent is deleted, then holds the delete until the poll returns.
type deletedDuringPollProvider struct {
	debrid.Client

	checking, gone, pollDone chan struct{}
	failed                   func() bool
	pollFailed               atomic.Bool
}

func (p *deletedDuringPollProvider) CheckStatus(*types.Torrent) (*types.Torrent, error) {
	close(p.checking)
	select {
	case <-p.gone:
		return nil, customerror.ErrTorrentNotFound
	case <-time.After(5 * time.Second):
		return nil, errors.New("the provider torrent was never deleted")
	}
}

func (p *deletedDuringPollProvider) DeleteTorrent(string) error {
	close(p.gone)
	<-p.pollDone
	p.pollFailed.Store(p.failed())
	return nil
}

// A removed download's provider torrent goes only after its queue record, so a poll seeing it gone does not fail the release.
func TestRemoveQueuedDuringStatusCheckFailsNothing(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{
		storage: store, queue: newQueue(store, "", nil, zerolog.Nop()), clients: xsync.NewMap[string, debrid.Client](),
		logger: zerolog.Nop(), processingEntries: xsync.NewMap[string, struct{}](),
	})
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release", Manual: true,
		Protocol: config.ProtocolTorrent, ActiveProvider: "provider", State: storage.EntryStateDownloading,
		Status: types.TorrentStatusDownloading,
		Providers: map[string]*storage.ProviderEntry{"provider": {
			Provider: "provider", ID: "id", Status: types.TorrentStatusDownloading,
		}},
	}
	provider := &deletedDuringPollProvider{
		checking: make(chan struct{}), gone: make(chan struct{}), pollDone: make(chan struct{}),
		failed: func() bool {
			saved, getErr := m.queue.GetTorrent(entry.InfoHash)
			return getErr == nil && saved.State == storage.EntryStateError
		},
	}
	m.clients.Store("provider", provider)
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	go func() {
		defer close(provider.pollDone)
		m.processQueuedTorrent(entry)
	}()
	wait := func(ch chan struct{}, what string) {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal(what)
		}
	}
	wait(provider.checking, "the poll never checked the provider")
	if removeErr := m.RemoveQueued(entry.InfoHash, false); removeErr != nil {
		t.Fatal(removeErr)
	}
	wait(provider.pollDone, "the poll never returned")
	wait(provider.gone, "the removal never deleted the provider torrent")
	if provider.pollFailed.Load() {
		t.Fatal("the poll failed the release whose provider torrent the removal deleted")
	}
	if m.queue.Contains(entry.InfoHash) {
		t.Fatal("the removed entry was written back")
	}
}
