package manager

import (
	"context"
	"strings"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// sharedHarness is a pack and two season entries that carry the same provider
// torrent, as convertToMultiSeason leaves them.
func sharedHarness(t *testing.T) *pruneHarness {
	t.Helper()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.mount2(t)
	for _, hash := range []string{"pack", "s1", "s2"} {
		h.addEntry(t, hash, shareID("shared"))
	}
	return h
}

func (h *pruneHarness) deleteEntry(t *testing.T, hash string) {
	t.Helper()
	if err := h.m.DeleteEntry(hash, true); err != nil {
		t.Fatal(err)
	}
	h.m.placementRuns.Wait()
}

func requireDeleted(t *testing.T, h *pruneHarness, want ...string) {
	t.Helper()
	got := h.provider.deletedIDs()
	if len(got) != len(want) {
		t.Fatalf("provider deletions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("provider deletions = %v, want %v", got, want)
		}
	}
}

// The shared provider torrent goes with its last holder, not with the first
// entry removed: the others would probe torrent_not_found and be reacquired.
func TestDeletingAnEntryKeepsAProviderTorrentAnotherEntryHolds(t *testing.T) {
	t.Parallel()
	h := sharedHarness(t)

	h.deleteEntry(t, "s1")
	h.deleteEntry(t, "s2")
	requireDeleted(t, h)

	h.deleteEntry(t, "pack")
	requireDeleted(t, h, "shared")
}

func TestInvalidatingASeasonLeavesItsSiblingsTorrent(t *testing.T) {
	t.Parallel()
	h := sharedHarness(t)
	job := reacquire.Job{
		EntryID:  "s1",
		Bindings: []reacquire.Binding{{EntryID: "s1", EntryFileID: "a.mkv"}},
	}

	if err := h.m.InvalidateReacquire(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	h.m.placementRuns.Wait()
	requireDeleted(t, h)
	for _, hash := range []string{"pack", "s2"} {
		if !h.has(hash) {
			t.Errorf("sibling %s was removed", hash)
		}
	}
	if h.has("s1") {
		t.Error("the invalidated season is still stored")
	}
}

func TestSwitchingAwayKeepsAPlacementACoHolderUses(t *testing.T) {
	t.Parallel()
	h := sharedHarness(t)
	entry, err := h.m.storage.Get("s1")
	if err != nil {
		t.Fatal(err)
	}

	h.m.dropSourcePlacement(entry, "torbox")
	requireDeleted(t, h)
	if entry.Providers["torbox"] != nil {
		t.Fatal("the switched entry still lists the source placement")
	}
	if err = h.m.storage.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}

	h.deleteEntry(t, "s2")
	h.deleteEntry(t, "pack")
	requireDeleted(t, h, "shared")
}

func TestReplacingAPlacementKeepsOneACoHolderUses(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{true, false} {
		client := &replacementProvider{}
		m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
		store, err := storage.NewStorage(t.TempDir(), storage.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		m.storage = store
		m.clients.Store("remaining", client)
		placement := func() map[string]*storage.ProviderEntry {
			return map[string]*storage.ProviderEntry{
				"remaining": {Provider: "remaining", ID: "old", Status: types.TorrentStatusDownloaded},
			}
		}
		entry := &storage.Entry{
			InfoHash: "0123456789012345678901234567890123456789", Name: "release",
			Protocol: config.ProtocolTorrent, ActiveProvider: "remaining", Providers: placement(),
		}
		if shared {
			sibling := &storage.Entry{InfoHash: "sibling", Name: "other", Providers: placement()}
			if addErr := store.AddOrUpdate(sibling); addErr != nil {
				t.Fatal(addErr)
			}
		}
		fixer := NewFixer(m)
		if _, moveErr := fixer.MoveTorrent(entry, "remaining", true); moveErr != nil {
			t.Fatal(moveErr)
		}
		m.placementRuns.Wait()
		if got, want := client.deletions.Load(), map[bool]int64{true: 0, false: 1}[shared]; got != want {
			t.Fatalf("shared=%v: provider deletions = %d, want %d", shared, got, want)
		}
	}
}

// A failed storage delete leaves the entry, so its provider torrent stays too:
// a library link bound to it must not point at a torrent that is gone.
func TestFailedStorageDeleteKeepsTheProviderTorrent(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.mount2(t)
	h.addEntry(t, "solo", nil)
	h.addEntry(t, "free", nil)
	var itemKey string
	for name := range h.m.storage.GetEntryItems() {
		if strings.HasSuffix(name, "solo") {
			itemKey = name
		}
	}
	if itemKey == "" {
		t.Fatal("the entry has no name index")
	}
	h.corruptStore(t, "items.db", itemKey, []byte{1, 2, 3})

	if err := h.m.DeleteEntry("solo", true); err == nil {
		t.Fatal("DeleteEntry() = nil, want the storage failure")
	}
	h.m.placementRuns.Wait()
	requireDeleted(t, h)
	if !h.has("solo") {
		t.Fatal("the entry is gone")
	}

	// The control: with the storage working, the same harness deletes the provider torrent of an entry it removes,
	// so the absence above is the order of the two deletes and nothing else.
	h.deleteEntry(t, "free")
	requireDeleted(t, h, "id-free")
}

// A queued entry that holds the provider torrent counts as a holder.
func TestAQueuedEntryCountsAsAHolder(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.mount2(t)
	h.addEntry(t, "stored", shareID("shared"))
	queued := &storage.Entry{
		InfoHash: "queued", Name: "queued", State: storage.EntryStateDownloading,
		Providers: map[string]*storage.ProviderEntry{"torbox": {Provider: "torbox", ID: "shared"}},
	}
	if err := h.m.queue.Add(queued); err != nil {
		t.Fatal(err)
	}

	h.deleteEntry(t, "stored")
	requireDeleted(t, h)

	if err := h.m.queue.Delete("queued", false, nil); err != nil {
		t.Fatal(err)
	}
	other := h.addEntry(t, "again", shareID("shared"))
	h.deleteEntry(t, other.InfoHash)
	requireDeleted(t, h, "shared")
}
