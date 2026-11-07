package manager

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func queuedEntryAddedAt(t *testing.T, provider debrid.Client, addedAt time.Time) (*Manager, *storage.Entry) {
	t.Helper()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{
		queue: newQueue(store, "", nil, zerolog.Nop()), clients: xsync.NewMap[string, debrid.Client](),
		logger: zerolog.Nop(), processingEntries: xsync.NewMap[string, struct{}](),
	})
	m.clients.Store("provider", provider)
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release",
		Protocol: config.ProtocolTorrent, ActiveProvider: "provider", State: storage.EntryStateDownloading,
		Providers: map[string]*storage.ProviderEntry{"provider": {Provider: "provider", ID: "id", AddedAt: addedAt}},
	}
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	return m, entry
}

// A provider that has not finished converting or linking a torrent is not a
// verdict on the release: the entry stays queued for the next pass and the
// provider's torrent is kept, until the wait has lasted maxProviderPendingTime.
func TestQueuedTorrentStatusTimeoutKeepsEntryUntilWaitIsExhausted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		age        time.Duration
		wantFailed bool
	}{
		{name: "fresh placement", age: time.Minute},
		{name: "wait exhausted", age: maxProviderPendingTime + time.Minute, wantFailed: true},
		{name: "unknown placement age", age: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := &failingStatusProvider{err: customerror.ErrProviderStatusTimeout}
			addedAt := time.Now().Add(-tc.age)
			if tc.age < 0 {
				addedAt = time.Time{}
			}
			m, entry := queuedEntryAddedAt(t, provider, addedAt)

			m.processQueuedTorrent(entry)

			saved, err := m.queue.GetTorrent(entry.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			if failed := saved.State == storage.EntryStateError; failed != tc.wantFailed {
				t.Fatalf("state = %q, want failed=%v", saved.State, tc.wantFailed)
			}
			if deleted := waitDeleted(provider); deleted != tc.wantFailed {
				t.Fatalf("provider torrent deleted = %v, want %v", deleted, tc.wantFailed)
			}
		})
	}
}

// waitDeleted reports whether the provider's torrent gets deleted; the manager
// deletes it in a goroutine, so a deletion is given a moment to arrive.
func waitDeleted(provider *failingStatusProvider) bool {
	deadline := time.Now().Add(time.Second)
	for provider.deleted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	return provider.deleted.Load() != 0
}

// submitProvider submits a torrent whose status check ends with err.
type submitProvider struct {
	debrid.Client

	err     error
	deleted atomic.Int64
}

func (*submitProvider) Config() config.Debrid {
	return config.Debrid{Name: "provider", Provider: "provider"}
}
func (*submitProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (*submitProvider) SubmitMagnet(t *types.Torrent) (*types.Torrent, error) {
	t.ID = "id"
	return t, nil
}

func (p *submitProvider) CheckStatus(t *types.Torrent) (*types.Torrent, error) { return t, p.err }

func (p *submitProvider) DeleteTorrent(string) error {
	p.deleted.Add(1)
	return nil
}

// A status check that gave up waiting must not surface as ErrTorrentNotCached,
// the only error submitToProvider reports to hearsay as "not cached".
func TestSubmitToProviderStatusTimeoutIsNotReportedAsUncached(t *testing.T) {
	t.Parallel()
	provider := &submitProvider{err: customerror.ErrProviderStatusTimeout}
	m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})

	_, err := m.submitToProvider(
		provider,
		&types.Torrent{InfoHash: "hash", Name: "name"},
		&ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}},
	)
	if !errors.Is(err, customerror.ErrProviderStatusTimeout) || errors.Is(err, customerror.ErrTorrentNotCached) {
		t.Fatalf("err = %v, want the status timeout and not ErrTorrentNotCached", err)
	}
}
