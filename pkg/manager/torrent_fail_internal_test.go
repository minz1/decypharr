package manager

import (
	"errors"
	"strings"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// The arr only warns on a qBit "error" torrent, so a bad release is failed through its API to be blocklisted and re-searched.
func TestFailBadReleaseFailsArrTorrentGrab(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit     func(*storage.Entry)
		wantFail bool
	}{
		"arr torrent":       {func(e *storage.Entry) { e.Protocol = config.ProtocolTorrent }, true},
		"manual torrent":    {func(e *storage.Entry) { e.Protocol, e.Manual = config.ProtocolTorrent, true }, false},
		"nzb":               {func(e *storage.Entry) { e.Protocol = config.ProtocolNZB }, false},
		"no configured arr": {func(e *storage.Entry) { e.Protocol, e.Category = config.ProtocolTorrent, "manual" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeArrRecovery{}
			m, entry := newDroppedTestManager(t, fake)
			tc.edit(entry)
			m.failBadRelease(entry, errors.New("entry in error state on debrid"))
			assertErrored(t, m)
			want := [3]string{"sonarr", strings.ToUpper(droppedHash), droppedHash}
			if got := len(fake.fails) == 1 && fake.fails[0] == want; got != tc.wantFail {
				t.Fatalf("FailDownload calls = %v, want failed = %v", fake.fails, tc.wantFail)
			}
		})
	}
}

// A failure decypharr caused itself (mount down, persist error) must not blocklist a good release.
func TestFailEntryLeavesTheGrabAlone(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, entry := newDroppedTestManager(t, fake)
	entry.Protocol = config.ProtocolTorrent
	m.failEntry(entry, errors.New("timeout waiting for mount files"))
	assertErrored(t, m)
	if len(fake.fails) != 0 {
		t.Fatalf("FailDownload calls = %v, want none", fake.fails)
	}
}

func TestFailBadReleaseWithoutRecoveryService(t *testing.T) {
	t.Parallel()
	m, entry := newDroppedTestManager(t, nil)
	entry.Protocol = config.ProtocolTorrent
	m.failBadRelease(entry, errors.New("debrid entry not found"))
	assertErrored(t, m)
}

func assertErrored(t *testing.T, m *Manager) {
	t.Helper()
	record, err := m.Queue().GetTorrent(droppedHash)
	if err != nil || record.State != storage.EntryStateError {
		t.Fatalf("record = %+v, %v; want errored", record, err)
	}
}

// Dead swarms surface as status-check errors; only those the provider pins on the torrent may blocklist the release.
func TestStatusCheckFailsGrabOnlyForDeadTorrents(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		provider *failingStatusProvider
		wantFail bool
	}{
		"provider marked it failed": {
			&failingStatusProvider{err: errors.New("torrent: x has error"), status: types.TorrentStatusError}, true,
		},
		"provider no longer has it": {&failingStatusProvider{err: customerror.ErrTorrentNotFound}, true},
		"provider status is error":  {&failingStatusProvider{status: types.TorrentStatusError}, true},
		"auth failure":              {&failingStatusProvider{err: errors.New("realdebrid API error: Status: 401")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeArrRecovery{}
			m, entry := newDroppedTestManager(t, fake)
			m.processingEntries = xsync.NewMap[string, struct{}]()
			m.store = testConfigStore(t, nil)
			m.clients.Store("provider", tc.provider)
			entry.Protocol, entry.ActiveProvider, entry.State = config.ProtocolTorrent, "provider", storage.EntryStateDownloading
			entry.Providers = map[string]*storage.ProviderEntry{"provider": {Provider: "provider", ID: "id"}}
			m.processQueuedTorrent(entry)
			assertErrored(t, m)
			if got := len(fake.fails) == 1; got != tc.wantFail {
				t.Fatalf("FailDownload calls = %v, want failed = %v", fake.fails, tc.wantFail)
			}
		})
	}
}
