package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func queuedEntryAddedAt(
	t *testing.T,
	provider debrid.Client,
	placementAddedAt, entryAddedOn time.Time,
) (*Manager, *storage.Entry) {
	t.Helper()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{
		queue: newQueue(store, nil, &grabIndex{}, zerolog.Nop()), clients: xsync.NewMap[string, debrid.Client](),
		logger: zerolog.Nop(),
	})
	m.clients.Store("provider", provider)
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "release",
		Protocol: config.ProtocolTorrent, ActiveProvider: "provider", State: storage.EntryStateDownloading,
		AddedOn: entryAddedOn,
		Providers: map[string]*storage.ProviderEntry{
			"provider": {Provider: "provider", ID: "id", AddedAt: placementAddedAt},
		},
	}
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	return m, entry
}

// A provider that has not finished converting or linking a torrent is not a verdict on the release: the poll reports
// what the provider waits on and nothing else, whatever the placement's age; the grab's deadline decides when the
// wait has lasted too long. A magnet still converting is a dead release once it does.
func TestAPollOfAPendingProviderReportsWhatItWaitsOn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		phase types.Phase
		err   error
		want  *pollSeen
	}{
		"finalizing":  {phase: types.PhaseFinalizing, want: &pollSeen{pending: grab.PendingProvider}},
		"converting":  {phase: types.PhaseConverting, want: &pollSeen{pending: grab.PendingConverting, dead: true}},
		"timed out":   {err: customerror.ErrProviderStatusTimeout, want: &pollSeen{pending: grab.PendingProvider}},
		"not convert": {err: notConverted(), want: &pollSeen{pending: grab.PendingConverting, dead: true}},
		"other":       {err: syscall.ECONNRESET},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			provider := &failingStatusProvider{err: tc.err, phase: tc.phase}
			m, entry := queuedEntryAddedAt(t, provider, time.Now().Add(-100*time.Hour), time.Time{})

			msg := poll(t.Context(), m, entry)
			closePlacements(t, m)

			if msg.ev != nil {
				t.Fatalf("event = %+v, want none", msg.ev)
			}
			if (msg.seen == nil) != (tc.want == nil) || tc.want != nil && *msg.seen != *tc.want {
				t.Fatalf("seen = %+v, want %+v", msg.seen, tc.want)
			}
		})
	}
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
func (*submitProvider) SubmitMagnet(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	t.ID = "id"
	return t, nil
}

func (p *submitProvider) CheckStatus(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	return t, p.err
}

func (p *submitProvider) DeleteTorrent(context.Context, string) error {
	p.deleted.Add(1)
	return nil
}

// A status check that gave up waiting is not a verdict. The add returns the
// submitted torrent as still downloading, so it is queued and the scheduler
// keeps checking it; the provider's torrent is not deleted, and the error is
// never ErrTorrentNotCached, the only one reported to hearsay as "not cached".
func TestSubmitToProviderStatusTimeoutQueuesTheTorrent(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"timeout":  customerror.ErrProviderStatusTimeout,
		"shutdown": errors.Join(customerror.ErrProviderStatusTimeout, errors.New("context canceled")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			provider := &submitProvider{err: err}
			m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})

			torrent, submitErr := m.submitToProvider(
				t.Context(),
				provider,
				&types.Torrent{InfoHash: "hash", Name: "name"},
				&ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}},
			)
			if submitErr != nil {
				t.Fatalf("err = %v, want the torrent queued", submitErr)
			}
			if torrent == nil || torrent.ID != "id" || torrent.Status != types.TorrentStatusDownloading {
				t.Fatalf("torrent = %#v, want the submitted torrent, downloading", torrent)
			}
			time.Sleep(20 * time.Millisecond)
			if provider.deleted.Load() != 0 {
				t.Fatal("a pending torrent was deleted at the provider")
			}
		})
	}
}

// notConverted is the error Real-Debrid returns while it is still converting a magnet.
func notConverted() error {
	return fmt.Errorf("torrent x still converting: %w: %w", customerror.ErrMagnetNotConverted,
		customerror.ErrProviderStatusTimeout)
}

// warnLines counts the WARN lines that name the entry's provider wait.
func warnLines(logs *bytes.Buffer) int {
	n := 0
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, `"level":"warn"`) && strings.Contains(line, "waiting on the provider") {
			n++
		}
	}
	return n
}

// A wait on the provider is logged when it starts and when it changes, with its age, not on every queue pass: live,
// one converting magnet logged a WARN per pass, 214 an hour.
func TestAProviderWaitIsLoggedOncePerChange(t *testing.T) {
	t.Parallel()
	provider := &failingStatusProvider{phase: types.PhaseConverting}
	m, entry := queuedEntryAddedAt(t, provider, time.Now().Add(-time.Hour), time.Time{})
	var logs bytes.Buffer
	m.logger = zerolog.New(&logs)

	for range 3 {
		poll(t.Context(), m, entry)
	}
	if got := warnLines(&logs); got != 1 {
		t.Fatalf("three passes on a converting magnet logged %d WARNs, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `"waited":`) {
		t.Fatalf("the wait's WARN carries no age:\n%s", logs.String())
	}

	provider.phase = types.PhaseFinalizing
	poll(t.Context(), m, entry)
	poll(t.Context(), m, entry)
	if got := warnLines(&logs); got != 2 {
		t.Fatalf("a changed wait logged %d WARNs in all, want 2:\n%s", got, logs.String())
	}
}

// pendingStatusProvider holds a status check until its ctx ends, then fails it the way a provider's Wait does when
// stopped: with the provider-pending error and the ctx error.
type pendingStatusProvider struct {
	debrid.Client

	wait     error
	checking chan struct{}
	deleted  atomic.Int64
}

func (p *pendingStatusProvider) Poll(
	ctx context.Context,
	t *types.Torrent,
	_ bool,
) (*types.Torrent, types.Observation, error) {
	close(p.checking)
	<-ctx.Done()
	return t, types.Observation{}, errors.Join(p.wait, ctx.Err())
}

func (p *pendingStatusProvider) DeleteTorrent(context.Context, string) error {
	p.deleted.Add(1)
	return nil
}

// pendingEntryManager is a manager with one queued torrent that has waited 7h on a provider whose status check holds
// until it is stopped.
func pendingEntryManager(
	t *testing.T,
	provider *pendingStatusProvider,
) (*Manager, *storage.Entry) {
	t.Helper()
	m, entry := newDroppedTestManager(t, nil)
	withLocalArr(m)
	m.store = testConfigStore(t, nil)
	m.clients.Store("provider", provider)
	entry.Protocol, entry.ActiveProvider, entry.State = config.ProtocolTorrent, "provider", storage.EntryStateDownloading
	entry.Providers = map[string]*storage.ProviderEntry{
		"provider": {Provider: "provider", ID: "id", AddedAt: time.Now().Add(-7 * time.Hour)},
	}
	if err := m.queue.Update(entry); err != nil {
		t.Fatal(err)
	}
	return m, entry
}

// A stop is no verdict (S1): a status check cut by the stop leaves the entry as it is, even one that has waited past
// its limit. Its grab is not failed, it is not marked errored, and the provider's torrent is not deleted.
func TestQueuedStopDuringCheckStatusLeavesEntry(t *testing.T) {
	t.Parallel()
	for name, wait := range map[string]error{
		"provider wait":        customerror.ErrProviderStatusTimeout,
		"magnet not converted": notConverted(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				provider := &pendingStatusProvider{wait: wait, checking: make(chan struct{})}
				m, entry := pendingEntryManager(t, provider)
				ctx, stop := context.WithCancel(t.Context())
				result := make(chan entryMsg, 1)
				go func() { result <- poll(ctx, m, entry) }()
				<-provider.checking
				stop()
				msg := <-result
				drainPlacements(t, m)

				saved, err := m.queue.GetTorrent(entry.InfoHash)
				if err != nil {
					t.Fatal(err)
				}
				if saved.State != storage.EntryStateDownloading || saved.LastError != "" {
					t.Fatalf("state = %q, last error %q; want the entry left downloading", saved.State, saved.LastError)
				}
				if msg.ev != nil || provider.deleted.Load() != 0 {
					t.Fatalf("result = %+v, provider deletes = %d; want none", msg.ev, provider.deleted.Load())
				}
			})
		})
	}
}

// poll runs entry's poll child once and returns what it reports.
func poll(ctx context.Context, m *Manager, entry *storage.Entry) entryMsg {
	return m.pollChild(ctx, entryChildCall{kind: grab.ChildPoll, grab: grab.Grab{Entry: entry}}, false)
}
