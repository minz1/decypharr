package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A provider at its active-download cap was retried every 30s forever; back off up to 10 minutes.
func TestThrottledRetryBacksOff(t *testing.T) {
	t.Parallel()
	for attempts, want := range map[int]time.Duration{
		0: 30 * time.Second, 1: time.Minute, 2: 2 * time.Minute, 4: 8 * time.Minute, 5: 10 * time.Minute, 60: 10 * time.Minute,
	} {
		if got := throttledRetryDelay(attempts); got != want {
			t.Errorf("throttledRetryDelay(%d) = %s, want %s", attempts, got, want)
		}
	}
}

type throttlingProvider struct {
	debrid.Client

	name    string
	err     error
	submits atomic.Int64
}

func (p *throttlingProvider) Config() config.Debrid {
	return config.Debrid{Name: p.name, Provider: p.name}
}
func (p *throttlingProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (p *throttlingProvider) SubmitMagnet(context.Context, *types.Torrent) (*types.Torrent, error) {
	p.submits.Add(1)
	return nil, p.err
}

// RD is tried first: its "blocked" must not hide TorBox's full slots behind it. The grab waits on the provider that
// throttled it, not on one that refused it for another reason.
func TestThrottledSubmitWaitsOnTheProviderThatThrottledIt(t *testing.T) {
	t.Parallel()
	m := withTestConfig(
		t,
		&Manager{work: testGroup(t), logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()},
	)
	m.clients.Store("realdebrid", &throttlingProvider{name: "realdebrid", err: customerror.ErrTorrentBlocked})
	m.clients.Store("torbox", &throttlingProvider{
		name: "torbox", err: fmt.Errorf("torbox queued it: %w", customerror.ErrTooManyActiveDownloads),
	})
	entry := &storage.Entry{InfoHash: "hash", Name: "name"}
	req := &ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}}
	msg := m.submitChild(t.Context(), entryChildCall{kind: grab.ChildSubmit, grab: grab.Grab{Entry: entry}, req: req})
	if msg.ev == nil || msg.ev.Kind != grab.EvSubmitResult || msg.ev.Outcome != grab.OutcomeThrottled {
		t.Fatalf("submit result = %+v, want a throttled submit result", msg.ev)
	}
	if !slices.Equal(msg.providers, []string{"torbox"}) {
		t.Fatalf("throttled providers = %v, want [torbox]", msg.providers)
	}
}

func linkedFiles() map[string]types.File {
	return map[string]types.File{
		"a.mkv": {Name: "a.mkv", ID: "1", Link: "https://rd/a"},
		"b.mkv": {Name: "b.mkv", ID: "2", Link: "https://rd/b"},
	}
}

func linksEntry(downloaded time.Duration) (*storage.Entry, *storage.ProviderEntry) {
	placement := &storage.ProviderEntry{Provider: "provider", ID: "id", Status: types.TorrentStatusDownloaded}
	if downloaded > 0 {
		at := time.Now().Add(-downloaded)
		placement.DownloadedAt = &at
	}
	entry := &storage.Entry{
		InfoHash:       "done",
		Name:           "done",
		ActiveProvider: "provider",
		Providers:      map[string]*storage.ProviderEntry{"provider": placement},
		Files:          map[string]*storage.File{},
	}
	return entry, placement
}

func TestLinksCheckedReadyWhenEveryFileHasALink(t *testing.T) {
	t.Parallel()
	m := &Manager{logger: zerolog.Nop()}
	entry, placement := linksEntry(0)
	finished := &types.Torrent{
		ID:     "id",
		Debrid: "provider",
		Status: types.TorrentStatusDownloaded,
		Files:  linkedFiles(),
	}
	msg := m.linksChecked(entry, placement, finished)
	if msg.ev == nil || msg.ev.Kind != grab.EvLinksReady || msg.update == nil {
		t.Fatalf("links result = %+v, want EvLinksReady with an update", msg)
	}
	msg.update(entry)
	if len(entry.Files) != 2 {
		t.Fatalf("entry files after the update = %d, want 2", len(entry.Files))
	}
	if got := entry.Providers["provider"]; got.DownloadedAt == nil || got.Progress != 1 {
		t.Fatalf("placement after the update = %+v, want finished", got)
	}
}

func TestLinksCheckedWaitsInsideTheBoundAndFailsPastIt(t *testing.T) {
	t.Parallel()
	partial := func() *types.Torrent {
		files := linkedFiles()
		file := files["b.mkv"]
		file.Link = ""
		files["b.mkv"] = file
		return &types.Torrent{ID: "id", Debrid: "provider", Status: types.TorrentStatusDownloaded, Files: files}
	}
	m := &Manager{logger: zerolog.Nop()}
	entry, placement := linksEntry(time.Minute)
	if msg := m.linksChecked(entry, placement, partial()); msg.ev != nil {
		t.Fatalf("links result inside the bound = %+v, want no result", msg.ev)
	}
	entry, placement = linksEntry(linksWaitLimit + time.Minute)
	msg := m.linksChecked(entry, placement, partial())
	if msg.ev == nil || msg.ev.Kind != grab.EvLinkWaitExpired || msg.ev.Fail != grab.FailBadRelease {
		t.Fatalf("links result past the bound = %+v, want EvLinkWaitExpired failing the release", msg.ev)
	}
}

func TestSubmitResultOfAPlacementCarriesItsTorrent(t *testing.T) {
	t.Parallel()
	entry := &storage.Entry{Files: map[string]*storage.File{"a.mkv": {}}}
	submitted := &types.Torrent{ID: "id", Debrid: "torbox", Status: types.TorrentStatusDownloading}
	msg := submitResult(entry, submitted, nil)
	if msg.ev == nil || msg.ev.Outcome != grab.OutcomePlacement || msg.ev.Provider != "torbox" ||
		msg.torrent != submitted {
		t.Fatalf("submit result = %+v, want a placement on torbox", msg)
	}
	msg = submitResult(entry, nil, errors.New("boom"))
	if msg.ev == nil || msg.ev.Outcome != grab.OutcomePermanent || msg.ev.Fail != grab.FailEntry {
		t.Fatalf("failed submit result = %+v, want a permanent failure", msg.ev)
	}
}
