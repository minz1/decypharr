package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
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
		queue: newQueue(store, "", nil, zerolog.Nop()), clients: xsync.NewMap[string, debrid.Client](),
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

// A provider that has not finished converting or linking a torrent is not a
// verdict on the release: the entry stays queued for the next pass and the
// provider's torrent is kept, until the wait has lasted maxProviderPendingTime.
// The wait is measured from the placement, else the entry; when neither knows
// its age it never runs out.
func TestQueuedTorrentStatusTimeoutKeepsEntryUntilWaitIsExhausted(t *testing.T) {
	t.Parallel()
	stale := time.Now().Add(-maxProviderPendingTime - time.Minute)
	fresh := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name                      string
		placementAddedAt, entryAt time.Time
		wantFailed                bool
	}{
		{name: "fresh placement", placementAddedAt: fresh, entryAt: stale},
		{name: "wait exhausted", placementAddedAt: stale, entryAt: fresh, wantFailed: true},
		{name: "no placement age, stale entry", entryAt: stale, wantFailed: true},
		{name: "no placement age, fresh entry", entryAt: fresh},
		{name: "no age anywhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := &failingStatusProvider{err: customerror.ErrProviderStatusTimeout}
			m, entry := queuedEntryAddedAt(t, provider, tc.placementAddedAt, tc.entryAt)

			msg := poll(t.Context(), m, entry)
			closePlacements(t, m)

			if failed := msg.ev != nil && msg.ev.Kind == grab.EvPendingDeadline; failed != tc.wantFailed {
				t.Fatalf("result = %+v, want failed=%v", msg.ev, tc.wantFailed)
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

// A magnet the provider has not converted within 2h (SM11-1, owner-proxy ruling; the ages are literal so the test pins
// the deadline, not just its use) has no seeders: its grab fails in the Arr, so the Arr searches another release, and
// the provider's torrent is deleted. Other pending waits keep the longer bound.
func TestUnconvertedMagnetFailsItsGrabAfterTheConversionWait(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err      error
		age      time.Duration
		wantFail bool
	}{
		"converting past 2h":              {notConverted(), 2*time.Hour + time.Minute, true},
		"converting within 2h":            {notConverted(), 2*time.Hour - time.Minute, false},
		"other pending wait past 2h":      {customerror.ErrProviderStatusTimeout, 2*time.Hour + time.Minute, false},
		"other pending wait past its own": {customerror.ErrProviderStatusTimeout, maxProviderPendingTime + time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, entry := newDroppedTestManager(t, nil)
			m.store = testConfigStore(t, nil)
			provider := &failingStatusProvider{err: tc.err}
			m.clients.Store("provider", provider)
			entry.Protocol, entry.ActiveProvider, entry.State = config.ProtocolTorrent, "provider", storage.EntryStateDownloading
			entry.Providers = map[string]*storage.ProviderEntry{
				"provider": {Provider: "provider", ID: "id", AddedAt: time.Now().Add(-tc.age)},
			}

			msg := poll(t.Context(), m, entry)
			closePlacements(t, m)

			failed := msg.ev != nil && msg.ev.Fail == grab.FailBadRelease
			if failed != tc.wantFail {
				t.Fatalf("result = %+v, want a bad release = %v", msg.ev, tc.wantFail)
			}
		})
	}
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
// one converting magnet logged a WARN per pass, 214 an hour. The give-up says how long the entry waited.
func TestAProviderWaitIsLoggedOncePerChange(t *testing.T) {
	t.Parallel()
	provider := &failingStatusProvider{err: notConverted()}
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

	provider.err = customerror.ErrProviderStatusTimeout // converted, now waiting on something else
	poll(t.Context(), m, entry)
	poll(t.Context(), m, entry)
	if got := warnLines(&logs); got != 2 {
		t.Fatalf("a changed wait logged %d WARNs in all, want 2:\n%s", got, logs.String())
	}

	provider.err = notConverted()
	entry.GetActiveProvider().AddedAt = time.Now().Add(-maxConversionWait - time.Minute)
	logs.Reset()
	poll(t.Context(), m, entry)
	if !strings.Contains(logs.String(), `"level":"error"`) || !strings.Contains(logs.String(), `"waited":`) {
		t.Fatalf("the give-up logged no ERROR with the wait's age:\n%s", logs.String())
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

func (p *pendingStatusProvider) CheckStatus(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	close(p.checking)
	<-ctx.Done()
	return t, errors.Join(p.wait, ctx.Err())
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
