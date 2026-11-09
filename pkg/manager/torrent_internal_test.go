package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// incompleteProvider reports a torrent whose files never gain download links.
type incompleteProvider struct {
	debrid.Client
}

func (incompleteProvider) GetTorrent(_ context.Context, id string) (*types.Torrent, error) {
	return &types.Torrent{
		ID: id, Debrid: "provider", InfoHash: "0123456789012345678901234567890123456789",
		Files: map[string]types.File{"video.mkv": {Name: "video.mkv"}},
	}, nil
}

func (incompleteProvider) UpdateTorrent(context.Context, *types.Torrent) error { return nil }

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
		&Manager{
			work:    testGroup(t),
			storage: store,
			clients: xsync.NewMap[string, debrid.Client](),
			logger:  zerolog.Nop(),
		},
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

	refreshed, err := m.refreshTorrent(t.Context(), entry.InfoHash)
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

func (c *countingStatusProvider) CheckStatus(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
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

func (f *failingStatusProvider) CheckStatus(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
	if f.status != "" {
		torrent.Status = f.status
	}
	return torrent, f.err
}

func (f *failingStatusProvider) DeleteTorrent(context.Context, string) error {
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
			m.processQueuedTorrent(t.Context(), entry)
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

func (p *removedDuringPollProvider) CheckStatus(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
	p.onCheck()
	torrent.Status = types.TorrentStatusDownloading
	return torrent, p.err
}

func (*removedDuringPollProvider) DeleteTorrent(context.Context, string) error { return nil }

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
			m.processQueuedTorrent(t.Context(), entry)
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

func (p *deletedDuringPollProvider) CheckStatus(context.Context, *types.Torrent) (*types.Torrent, error) {
	close(p.checking)
	select {
	case <-p.gone:
		return nil, customerror.ErrTorrentNotFound
	case <-time.After(5 * time.Second):
		return nil, errors.New("the provider torrent was never deleted")
	}
}

func (p *deletedDuringPollProvider) DeleteTorrent(context.Context, string) error {
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
		m.processQueuedTorrent(t.Context(), entry)
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

type removedDuringSubmitProvider struct {
	*fakePruneProvider

	submitted func()
}

func (*removedDuringSubmitProvider) Config() config.Debrid {
	return config.Debrid{Name: "provider", Provider: "provider"}
}
func (*removedDuringSubmitProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (p *removedDuringSubmitProvider) SubmitMagnet(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	p.submitted()
	return &types.Torrent{
		ID: "provider-id", Debrid: "provider", InfoHash: t.InfoHash, Name: t.Name,
		Status: types.TorrentStatusDownloading, Files: map[string]types.File{},
	}, nil
}
func (*removedDuringSubmitProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	return t, nil
}

// An entry the arr removes while its submit is in flight is not re-created, and the provider torrent goes with it.
func TestTorrentRemovedDuringSubmitLeavesNothing(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, nil)
	hash := "0123456789012345678901234567890123456789"
	fake := &removedDuringSubmitProvider{fakePruneProvider: &fakePruneProvider{}, submitted: func() {
		if err := m.RemoveQueued(hash, false); err != nil {
			t.Error(err)
		}
	}}
	m.clients.Store("provider", fake)
	entry := &storage.Entry{
		InfoHash: hash, Name: "release", Category: "sonarr", Protocol: config.ProtocolTorrent,
		State: storage.EntryStateDownloading, Status: types.TorrentStatusQueued,
		Files: map[string]*storage.File{}, Providers: map[string]*storage.ProviderEntry{},
	}
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	req := &ImportRequest{Magnet: &utils.Magnet{InfoHash: hash, Name: "release"}, Arr: arr.Arr{Name: "sonarr"}}

	if err := m.processTorrentJob(
		t.Context(),
		&Job{ID: hash, Type: JobTypeTorrent, Entry: entry, Request: req},
	); err != nil {
		t.Fatal(err)
	}

	if m.queue.Contains(hash) {
		t.Fatal("an entry removed during its submit was written back")
	}
	if got := fake.deletedIDs(); !slices.Equal(got, []string{"provider-id"}) {
		t.Fatalf("deleted provider torrents = %v, want the one just added", got)
	}
}

// A post-download action that finishes after the arr removed its entry does not save it back as completed.
func TestCompleteEntryLeavesARemovedEntryRemoved(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release", Protocol: config.ProtocolTorrent,
		State: storage.EntryStateDownloading, Status: types.TorrentStatusDownloaded,
	}
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	if err := m.queue.Delete(entry.InfoHash, false, nil); err != nil {
		t.Fatal(err)
	}

	if err := m.downloader.completeEntry(t.Context(), entry); err != nil {
		t.Fatal(err)
	}

	if m.queue.Contains(entry.InfoHash) {
		t.Fatal("a removed entry was saved back as completed")
	}
}

// stoppedListing stops the sync during its listing and answers with the page it had read so far and the ctx error.
type stoppedListing struct {
	*fakeListing

	stop context.CancelFunc
}

func (l *stoppedListing) GetTorrents(ctx context.Context) ([]*types.Torrent, error) {
	l.stop()
	return l.torrents, ctx.Err()
}

// A listing cut by the stop is an error, not a short list (S5): it must not count the entries it lacks as misses, or
// look them up, or drop them.
func TestRefreshStopMidListingRecordsNoDrops(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, _ := newDroppedTestManager(t, fake)
	addPlaced(t, m, 1)
	full := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}
	syncTimes(t, m, full, 1) // the first miss is on record
	ctx, stop := context.WithCancel(t.Context())

	err := m.doRefreshTorrents(ctx, "rd", &stoppedListing{fakeListing: full, stop: stop})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the stop", err)
	}
	if !stored(m, "hash-00") || len(fake.fails) != 0 || full.lookups() != 0 {
		t.Fatalf("a cut listing acted on a miss: stored=%v fails=%d lookups=%d",
			stored(m, "hash-00"), len(fake.fails), full.lookups())
	}
}

// A lookup that finished as the sync was stopped is no confirmation: the placement is not dropped (S1).
func TestConfirmDropsStoppedLookupDropsNothing(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, _ := newDroppedTestManager(t, fake)
	addPlaced(t, m, 1)
	ctx, stop := context.WithCancel(t.Context())
	client := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: func(string) (*types.Torrent, error) {
		stop()
		return nil, customerror.ErrTorrentNotFound
	}}
	syncTimes(t, m, client, 1) // the first miss is on record

	syncTimesCtx(ctx, t, m, client, 1)

	if !stored(m, "hash-00") || len(fake.fails) != 0 {
		t.Fatalf("a stopped lookup dropped the entry: stored=%v fails=%d", stored(m, "hash-00"), len(fake.fails))
	}
}

// stoppedUpdateProvider fails every file update the way a provider does once the sync's ctx has ended.
type stoppedUpdateProvider struct {
	debrid.Client
}

func (stoppedUpdateProvider) UpdateTorrent(ctx context.Context, _ *types.Torrent) error {
	return fmt.Errorf("update: %w", ctx.Err())
}

// A sync stopped with torrents left to process drains them quietly: the stop is no failure of each torrent, so it logs
// no ERROR per remaining one (S5).
func TestProcessNewTorrentsCancelledDrainsQuietly(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	m := withTestConfig(t, &Manager{
		clients: xsync.NewMap[string, debrid.Client](),
		logger:  zerolog.New(zerolog.SyncWriter(&logs)),
	})
	m.clients.Store("provider", stoppedUpdateProvider{})
	var pending []*types.Torrent
	for i := range 40 {
		id := fmt.Sprintf("id-%02d", i)
		pending = append(pending, &types.Torrent{ID: id, InfoHash: id, Debrid: "provider"})
	}
	ctx, stop := context.WithCancel(t.Context())
	stop()

	if err := m.processNewTorrents(ctx, "provider", pending); err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(logs.String(), `"level":"error"`); got > 1 {
		t.Fatalf(
			"a stopped sync of %d torrents logged %d ERRORs, want at most 1:\n%s",
			len(pending),
			got,
			logs.String(),
		)
	}
}
