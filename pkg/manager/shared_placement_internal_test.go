package manager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

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
	drainPlacements(t, h.m)
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
	synctest.Test(t, func(t *testing.T) {
		h := sharedHarness(t)

		h.deleteEntry(t, "s1")
		h.deleteEntry(t, "s2")
		requireDeleted(t, h)

		h.deleteEntry(t, "pack")
		requireDeleted(t, h, "shared")
	})
}

func TestInvalidatingASeasonLeavesItsSiblingsTorrent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := sharedHarness(t)
		job := reacquire.Job{
			EntryID:  "s1",
			Bindings: []reacquire.Binding{{EntryID: "s1", EntryFileID: "a.mkv"}},
		}

		if err := h.m.InvalidateReacquire(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		drainPlacements(t, h.m)
		requireDeleted(t, h)
		for _, hash := range []string{"pack", "s2"} {
			if !h.has(hash) {
				t.Errorf("sibling %s was removed", hash)
			}
		}
		if h.has("s1") {
			t.Error("the invalidated season is still stored")
		}
	})
}

func TestSwitchingAwayKeepsAPlacementACoHolderUses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := sharedHarness(t)
		entry, err := h.m.storage.Get("s1")
		if err != nil {
			t.Fatal(err)
		}

		h.m.dropSourcePlacement(context.Background(), entry, "torbox")
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
	})
}

func TestReplacingAPlacementKeepsOneACoHolderUses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
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
			drainPlacements(t, m)
			if got, want := client.deletions.Load(), map[bool]int64{true: 0, false: 1}[shared]; got != want {
				t.Fatalf("shared=%v: provider deletions = %d, want %d", shared, got, want)
			}
		}
	})
}

// A failed storage delete leaves the entry, so its provider torrent stays too:
// a library link bound to it must not point at a torrent that is gone.
func TestFailedStorageDeleteKeepsTheProviderTorrent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
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
		drainPlacements(t, h.m)
		requireDeleted(t, h)
		if !h.has("solo") {
			t.Fatal("the entry is gone")
		}

		// The control: with the storage working, the same harness deletes the provider torrent of an entry it removes,
		// so the absence above is the order of the two deletes and nothing else.
		h.deleteEntry(t, "free")
		requireDeleted(t, h, "id-free")
	})
}

// Stop waits for the provider deletions in flight, and refuses new ones from then on: one started after the wait
// would read a storage that is about to close.
func TestPlacementRunsEndBeforeTheStorageCloses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newPruneHarness(t, config.PruneModeDelete)
		release := make(chan struct{})
		finished := make(chan struct{})
		if !h.m.startPlacementRun("test entry", "torbox", func(context.Context) {
			<-release
			close(finished)
		}) {
			t.Fatal("a run was refused before Stop")
		}

		closed := make(chan struct{})
		go func() {
			closePlacements(t, h.m)
			close(closed)
		}()
		select {
		case <-closed:
			t.Fatal("closing the runs did not wait for the one in flight")
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		<-closed
		select {
		case <-finished:
		default:
			t.Fatal("the run in flight did not finish before the runs closed")
		}

		ran := false
		if h.m.startPlacementRun("test entry", "torbox", func(context.Context) { ran = true }) {
			t.Fatal("a run was accepted after the runs closed")
		}
		drainPlacements(t, h.m)
		if ran {
			t.Fatal("a refused run touched the storage")
		}
	})
}

// Stop itself waits for a provider deletion in flight before it closes the storage the deletion reads, and refuses
// one that is requested after.
func TestStopWaitsForAPlacementRunBeforeClosingStorage(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newPruneHarness(t, config.PruneModeDelete)
		h.addEntry(t, "stored", nil)
		wireLifecycle(t, h.m) // the group Stop runs, with the task that closes the storage last
		release := make(chan struct{})
		var readErr error
		if !h.m.startPlacementRun("test entry", "torbox", func(context.Context) {
			<-release
			_, readErr = h.m.storage.Get("stored")
		}) {
			t.Fatal("a run was refused before Stop")
		}
		stopped := make(chan struct{})
		go func() {
			_ = h.m.Stop(t.Context())
			close(stopped)
		}()

		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned, and closed the storage, while a placement run was in flight")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Fatal("Stop did not finish once the run ended")
		}
		if readErr != nil {
			t.Fatalf("the run read a closed storage: %v", readErr)
		}
		if h.m.startPlacementRun(
			"test entry",
			"torbox",
			func(context.Context) { t.Error("a run started after Stop") },
		) {
			t.Fatal("a run was accepted after Stop")
		}
	})
}

// A deletion refused because the manager is stopping leaves a provider torrent behind: that is said, with the
// entry and the provider, so it is not orphaned silently.
func TestRefusedPlacementRunIsLogged(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	closePlacements(t, h.m)

	if h.m.startPlacementRun("Show.S01", "torbox", func(context.Context) {}) {
		t.Fatal("a run was accepted after the runs closed")
	}
	logged := h.logs.String()
	for _, want := range []string{`"level":"warn"`, "Show.S01", "torbox"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log %q does not carry %q", logged, want)
		}
	}
}

// A removal that arrives while the manager stops is not run against a closed storage.
func TestDeleteEntryAfterStopDoesNotRunPlacementRemoval(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newPruneHarness(t, config.PruneModeDelete)
		h.mount2(t)
		h.addEntry(t, "late", nil)
		closePlacements(t, h.m)

		if err := h.m.DeleteEntry("late", true); err != nil {
			t.Fatal(err)
		}
		drainPlacements(t, h.m)
		requireDeleted(t, h)
	})
}

// duplicateAddProvider answers every add with the ID of a torrent it already holds, as a provider does for a
// magnet that is already in the account, and then fails the status check.
type duplicateAddProvider struct {
	debrid.Client

	id string
}

func (c *duplicateAddProvider) Config() config.Debrid {
	return config.Debrid{Name: "torbox", Provider: "torbox"}
}

func (c *duplicateAddProvider) SubmitMagnet(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
	torrent.ID = c.id
	return torrent, nil
}

func (c *duplicateAddProvider) CheckStatus(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
	return torrent, errors.New("status check failed")
}

// A torrent a failed add or status check discards may be one another entry holds: the provider answers a duplicate
// add with the existing ID.
func TestDiscardingASubmittedTorrentKeepsOneACoHolderUses(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{true, false} {
		h := newPruneHarness(t, config.PruneModeDelete)
		h.mount2(t)
		if held {
			h.addEntry(t, "holder", shareID("dup"))
		}

		h.m.discardSubmitted(context.Background(), "torbox", "dup", "self")
		if held {
			requireDeleted(t, h)
		} else {
			requireDeleted(t, h, "dup")
		}
	}
}

func TestFixerReplacementThatFailsItsStatusCheckKeepsASharedTorrent(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{true, false} {
		h := newPruneHarness(t, config.PruneModeDelete)
		h.mount2(t)
		if held {
			h.addEntry(t, "holder", shareID("dup"))
		}
		h.m.config = h.m.store.Get()
		client := &duplicateAddProvider{Client: h.provider, id: "dup"}
		h.m.clients.Store("torbox", client)
		entry := &storage.Entry{InfoHash: "0123456789012345678901234567890123456789", Name: "release"}

		if _, err := NewFixer(h.m).submitReplacement(client, "torbox", entry); err == nil {
			t.Fatal("submitReplacement() = nil, want the status check failure")
		}
		if held {
			requireDeleted(t, h)
		} else {
			requireDeleted(t, h, "dup")
		}
	}
}

// A queued entry that holds the provider torrent counts as a holder.
func TestAQueuedEntryCountsAsAHolder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
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
	})
}
