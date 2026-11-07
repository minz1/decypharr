package manager

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
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
		logger: zerolog.Nop(), processingEntries: xsync.NewMap[string, struct{}](),
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
// its age the first timeout starts the clock.
func TestQueuedTorrentStatusTimeoutKeepsEntryUntilWaitIsExhausted(t *testing.T) {
	t.Parallel()
	stale := time.Now().Add(-maxProviderPendingTime - time.Minute)
	fresh := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name                      string
		placementAddedAt, entryAt time.Time
		wantFailed                bool
		wantClockStarted          bool
	}{
		{name: "fresh placement", placementAddedAt: fresh, entryAt: stale},
		{name: "wait exhausted", placementAddedAt: stale, entryAt: fresh, wantFailed: true},
		{name: "no placement age, stale entry", entryAt: stale, wantFailed: true},
		{name: "no placement age, fresh entry", entryAt: fresh},
		{name: "no age anywhere starts the clock", wantClockStarted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := &failingStatusProvider{err: customerror.ErrProviderStatusTimeout}
			m, entry := queuedEntryAddedAt(t, provider, tc.placementAddedAt, tc.entryAt)

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
			if started := !saved.GetActiveProvider().AddedAt.IsZero(); tc.wantClockStarted && !started {
				t.Fatal("the first timeout did not record when the wait began")
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
			fake := &fakeArrRecovery{}
			m, entry := newDroppedTestManager(t, fake)
			m.processingEntries = xsync.NewMap[string, struct{}]()
			m.store = testConfigStore(t, nil)
			provider := &failingStatusProvider{err: tc.err}
			m.clients.Store("provider", provider)
			entry.Protocol, entry.ActiveProvider, entry.State = config.ProtocolTorrent, "provider", storage.EntryStateDownloading
			entry.Providers = map[string]*storage.ProviderEntry{
				"provider": {Provider: "provider", ID: "id", AddedAt: time.Now().Add(-tc.age)},
			}

			m.processQueuedTorrent(entry)

			if got := len(fake.fails) == 1; got != tc.wantFail {
				t.Fatalf("FailDownload calls = %v, want failed = %v", fake.fails, tc.wantFail)
			}
			if tc.wantFail {
				assertErrored(t, m)
				if !waitDeleted(provider) {
					t.Fatal("the unconverted magnet was not deleted at the provider")
				}
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
		m.processQueuedTorrent(entry)
	}
	if got := warnLines(&logs); got != 1 {
		t.Fatalf("three passes on a converting magnet logged %d WARNs, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `"waited":`) {
		t.Fatalf("the wait's WARN carries no age:\n%s", logs.String())
	}

	provider.err = customerror.ErrProviderStatusTimeout // converted, now waiting on something else
	m.processQueuedTorrent(entry)
	m.processQueuedTorrent(entry)
	if got := warnLines(&logs); got != 2 {
		t.Fatalf("a changed wait logged %d WARNs in all, want 2:\n%s", got, logs.String())
	}

	provider.err = notConverted()
	entry.GetActiveProvider().AddedAt = time.Now().Add(-maxConversionWait - time.Minute)
	logs.Reset()
	m.processQueuedTorrent(entry)
	if !strings.Contains(logs.String(), `"level":"error"`) || !strings.Contains(logs.String(), `"waited":`) {
		t.Fatalf("the give-up logged no ERROR with the wait's age:\n%s", logs.String())
	}
}
