package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func dropEntry(providers ...string) *storage.Entry {
	e := &storage.Entry{InfoHash: "h1", Providers: map[string]*storage.ProviderEntry{}}
	for _, p := range providers {
		e.Providers[p] = &storage.ProviderEntry{Provider: p}
	}
	return e
}

func TestDropNeedsTwoConsecutiveMisses(t *testing.T) {
	t.Parallel()
	var c torrentChanges
	e := dropEntry("A")
	c.classify("A", e, nil, false)
	if len(c.delete) != 0 || len(c.update) != 0 {
		t.Fatalf("first miss acted: %+v", c)
	}
	if _, ok := c.misses["h1"]; !ok {
		t.Fatal("first miss not recorded")
	}
	if _, ok := e.Providers["A"]; !ok {
		t.Fatal("provider removed on first miss")
	}
	var c2 torrentChanges
	c2.classify("A", e, nil, true)
	if len(c2.confirm) != 1 || c2.confirm[0] != e || len(c2.delete) != 0 {
		t.Fatalf("second miss is not a confirmation candidate: %+v", c2)
	}
	if _, ok := e.Providers["A"]; !ok {
		t.Fatal("provider removed before confirmation")
	}
}

func TestMultiProviderDropNeedsTwoMisses(t *testing.T) {
	t.Parallel()
	e := dropEntry("A", "B")
	var c torrentChanges
	c.classify("A", e, nil, false)
	if len(c.update) != 0 || e.Providers["A"] == nil {
		t.Fatalf("first miss acted: %+v", c)
	}
	var c2 torrentChanges
	c2.classify("A", e, nil, true)
	if len(c2.confirm) != 1 || len(c2.update) != 0 || len(c2.delete) != 0 || e.Providers["A"] == nil {
		t.Fatalf("second miss: %+v", c2)
	}
}

func TestReappearingTorrentResetsMiss(t *testing.T) {
	t.Parallel()
	var c torrentChanges
	c.classify("A", dropEntry("A"), &types.Torrent{InfoHash: "h1"}, true)
	if _, ok := c.misses["h1"]; ok {
		t.Fatal("present torrent recorded as miss")
	}
}

// fakeListing is a provider whose listing and per-ID lookup are scripted.
type fakeListing struct {
	debrid.Client

	limit    int
	torrents []*types.Torrent

	mu     sync.Mutex
	lookup func(id string) (*types.Torrent, error)
	looked []string
}

func (f *fakeListing) GetTorrents() ([]*types.Torrent, error) { return f.torrents, nil }
func (f *fakeListing) Config() config.Debrid                  { return config.Debrid{Limit: f.limit} }

func (f *fakeListing) GetTorrent(id string) (*types.Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.looked = append(f.looked, id)
	return f.lookup(id)
}

func (f *fakeListing) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.looked)
}

func goneLookup(string) (*types.Torrent, error) { return nil, customerror.ErrTorrentNotFound }

// otherListed is a listed torrent no stored entry has; sync skips it because
// its provider has no client.
func otherListed() *types.Torrent { return &types.Torrent{ID: "other-id", InfoHash: "other-hash"} }

// addPlaced stores n entries placed on provider "rd" (IDs id-0..) with queue records.
func addPlaced(t *testing.T, m *Manager, n int) {
	t.Helper()
	for i := range n {
		hash := fmt.Sprintf("hash-%02d", i)
		entry := &storage.Entry{
			InfoHash: hash, Name: hash, Category: "sonarr", State: storage.EntryStatePausedUP,
			Providers: map[string]*storage.ProviderEntry{
				"rd": {Provider: "rd", ID: fmt.Sprintf("id-%02d", i)},
			},
		}
		if err := m.queue.Add(entry); err != nil {
			t.Fatal(err)
		}
		if err := m.storage.AddOrUpdate(entry); err != nil {
			t.Fatal(err)
		}
	}
}

func syncTimes(t *testing.T, m *Manager, client debrid.Client, n int) {
	t.Helper()
	for range n {
		if err := m.doRefreshTorrents(context.Background(), "rd", client); err != nil {
			t.Fatal(err)
		}
	}
}

func stored(m *Manager, hash string) bool {
	_, err := m.storage.Get(hash)
	return err == nil
}

func TestRefreshDropsOnlyAfterTwoListingsAndNotFound(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, _ := newDroppedTestManager(t, fake)
	addPlaced(t, m, 1)
	client := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}

	syncTimes(t, m, client, 1)
	if !stored(m, "hash-00") || client.lookups() != 0 {
		t.Fatalf("first miss acted: stored=%v lookups=%d", stored(m, "hash-00"), client.lookups())
	}
	syncTimes(t, m, client, 1)
	if stored(m, "hash-00") {
		t.Fatal("entry survived a confirmed drop")
	}
	if len(client.looked) != 1 || client.looked[0] != "id-00" {
		t.Fatalf("lookups=%v", client.looked)
	}
	if len(fake.fails) != 1 {
		t.Fatalf("FailDownload calls=%d, want 1", len(fake.fails))
	}
}

func TestRefreshKeepsPlacementListedByID(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, _ := newDroppedTestManager(t, fake)
	addPlaced(t, m, 1)
	client := &fakeListing{
		torrents: []*types.Torrent{{ID: "id-00", InfoHash: "synthetic-hash"}},
		lookup:   goneLookup,
	}

	syncTimes(t, m, client, 3)

	if !stored(m, "hash-00") || len(fake.fails) != 0 || client.lookups() != 0 {
		t.Fatalf("listed-by-ID entry acted on: stored=%v fails=%d lookups=%d",
			stored(m, "hash-00"), len(fake.fails), client.lookups())
	}
}

func TestRefreshSkipsDropsWhenListingHitsLimit(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, _ := newDroppedTestManager(t, fake)
	addPlaced(t, m, 1)
	client := &fakeListing{
		limit:    2,
		torrents: []*types.Torrent{otherListed(), {ID: "other-2", InfoHash: "other-hash-2"}},
		lookup:   goneLookup,
	}

	syncTimes(t, m, client, 3)

	if !stored(m, "hash-00") || len(fake.fails) != 0 || client.lookups() != 0 {
		t.Fatalf("truncated listing dropped an entry: fails=%d lookups=%d", len(fake.fails), client.lookups())
	}
}

func TestRefreshKeepsDropWhenGetTorrentFindsIt(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, _ := newDroppedTestManager(t, fake)
	addPlaced(t, m, 1)
	client := &fakeListing{
		torrents: []*types.Torrent{otherListed()},
		lookup:   func(id string) (*types.Torrent, error) { return &types.Torrent{ID: id}, nil },
	}

	syncTimes(t, m, client, 3)

	if !stored(m, "hash-00") || len(fake.fails) != 0 {
		t.Fatal("entry still on the provider was dropped")
	}
	if client.lookups() != 1 {
		t.Fatalf("lookups=%d, want 1 (the miss is cleared once found)", client.lookups())
	}
}

func TestRefreshRetriesDropOnLookupError(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, _ := newDroppedTestManager(t, fake)
	addPlaced(t, m, 1)
	client := &fakeListing{
		torrents: []*types.Torrent{otherListed()},
		lookup:   func(string) (*types.Torrent, error) { return nil, errors.New("timeout") },
	}

	syncTimes(t, m, client, 2)
	if !stored(m, "hash-00") || len(fake.fails) != 0 {
		t.Fatal("unconfirmed drop was applied")
	}

	client.lookup = goneLookup
	syncTimes(t, m, client, 1)
	if stored(m, "hash-00") || len(fake.fails) != 1 {
		t.Fatalf("retry did not drop: stored=%v fails=%d", stored(m, "hash-00"), len(fake.fails))
	}
}

func TestRefreshCapsDropRecoveries(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, _ := newDroppedTestManager(t, fake)
	addPlaced(t, m, 15)
	client := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}

	syncTimes(t, m, client, 2)
	remaining := 0
	for i := range 15 {
		if stored(m, fmt.Sprintf("hash-%02d", i)) {
			remaining++
		}
	}
	if remaining != 5 || len(fake.fails) != maxDropRecoveriesPerSync {
		t.Fatalf("remaining=%d fails=%d, want 5 and %d", remaining, len(fake.fails), maxDropRecoveriesPerSync)
	}

	syncTimes(t, m, client, 1)
	if len(fake.fails) != 15 || stored(m, "hash-00") {
		t.Fatalf("deferred drops not applied next sync: fails=%d", len(fake.fails))
	}
}
