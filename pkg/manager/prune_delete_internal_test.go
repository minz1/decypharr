package manager

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type fakePruneProvider struct {
	debrid.Client

	mu        sync.Mutex
	deleted   []string
	looked    []string
	deleteErr func(id string) error
	lookup    func(id string) (*types.Torrent, error)
	onDelete  func(id string)
}

func (f *fakePruneProvider) DeleteTorrent(id string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	hook, fail := f.onDelete, f.deleteErr
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if fail != nil {
		return fail(id)
	}
	return nil
}

func (f *fakePruneProvider) GetTorrent(id string) (*types.Torrent, error) {
	f.mu.Lock()
	f.looked = append(f.looked, id)
	lookup := f.lookup
	f.mu.Unlock()
	if lookup != nil {
		return lookup(id)
	}
	return nil, customerror.ErrTorrentNotFound
}

func (f *fakePruneProvider) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted, f.looked = nil, nil
}

func (f *fakePruneProvider) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakePruneProvider) deletedHashes() []string {
	var out []string
	for _, id := range f.deletedIDs() {
		out = append(out, strings.TrimPrefix(id, "id-"))
	}
	return out
}

type countingMount struct {
	MountManager

	mu    sync.Mutex
	calls int
}

func (c *countingMount) Refresh([]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil
}

func (c *countingMount) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (h *pruneHarness) mount2(t *testing.T) *countingMount {
	t.Helper()
	cm := &countingMount{}
	h.m.mountManager = cm
	return cm
}

func (h *pruneHarness) cands(t *testing.T, hashes ...string) []PruneCandidate {
	t.Helper()
	var out []PruneCandidate
	for _, hash := range hashes {
		if err := h.m.storage.SetUnreferencedSince(hash, h.now); err != nil {
			t.Fatal(err)
		}
		out = append(out, PruneCandidate{InfoHash: hash, Provider: "torbox", UnreferencedSince: h.now})
	}
	return out
}

func (h *pruneHarness) has(hash string) bool {
	_, err := h.m.storage.Get(hash)
	return err == nil
}

func (h *pruneHarness) ledgerHas(t *testing.T, hash string) bool {
	t.Helper()
	_, ok := h.ledger(t)[hash]
	return ok
}

func shareID(id string) func(*storage.Entry) {
	return func(e *storage.Entry) { e.Providers["torbox"].ID = id }
}

func TestPruneDeletesPlacementThenRecord(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	cs := h.cands(t, "aaaa")

	deleted, failed, freed := h.m.pruneEntries(context.Background(), cs)

	if deleted != 1 || failed != 0 || freed != 100 {
		t.Fatalf("deleted=%d failed=%d freed=%d", deleted, failed, freed)
	}
	if ids := h.provider.deletedIDs(); len(ids) != 1 || ids[0] != "id-aaaa" {
		t.Fatalf("provider calls %v", ids)
	}
	if h.has("aaaa") || h.ledgerHas(t, "aaaa") {
		t.Fatal("entry or ledger key survived")
	}
	if len(h.fake.deletedBindings) != 0 {
		t.Fatalf("bindings touched: %v", h.fake.deletedBindings)
	}
}

func TestPruneSharedPlacementWaitsForLastHolder(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", shareID("42"))
	h.addEntry(t, "bbbb", shareID("42"))

	deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))
	if deleted != 1 || failed != 0 || len(h.provider.deletedIDs()) != 0 || h.has("aaaa") || !h.has("bbbb") {
		t.Fatalf("A: deleted=%d failed=%d calls=%v", deleted, failed, h.provider.deletedIDs())
	}
	deleted, failed, _ = h.m.pruneEntries(context.Background(), h.cands(t, "bbbb"))
	if deleted != 1 || failed != 0 || len(h.provider.deletedIDs()) != 1 || h.provider.deletedIDs()[0] != "42" {
		t.Fatalf("B: deleted=%d failed=%d calls=%v", deleted, failed, h.provider.deletedIDs())
	}
}

func TestPruneSharedPlacementInOneBatchDeletesOnce(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", shareID("42"))
	h.addEntry(t, "bbbb", shareID("42"))

	deleted, failed, freed := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa", "bbbb"))

	if deleted != 2 || failed != 0 || freed != 100 || len(h.provider.deletedIDs()) != 1 {
		t.Fatalf("deleted=%d failed=%d freed=%d calls=%v", deleted, failed, freed, h.provider.deletedIDs())
	}
}

func TestPruneProviderErrorKeepsEntry(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	h.provider.deleteErr = func(string) error { return errors.New("500") }
	h.provider.lookup = func(id string) (*types.Torrent, error) { return &types.Torrent{ID: id}, nil }

	deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))

	if deleted != 0 || failed != 1 || !h.has("aaaa") || !h.ledgerHas(t, "aaaa") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneLookupErrorKeepsEntry(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	h.provider.deleteErr = func(string) error { return errors.New("500") }
	h.provider.lookup = func(string) (*types.Torrent, error) { return nil, errors.New("timeout") }

	deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))

	if deleted != 0 || failed != 1 || !h.has("aaaa") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneNotFoundCountsAsDeleted(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	h.provider.deleteErr = func(string) error { return errors.New("404") }

	deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))

	if deleted != 1 || failed != 0 || h.has("aaaa") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneUnconfiguredProviderKeepsEntry(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	h.m.clients.Delete("torbox")

	deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))

	if deleted != 0 || failed != 1 || !h.has("aaaa") || !h.ledgerHas(t, "aaaa") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func usenetEntry(e *storage.Entry) {
	e.ActiveProvider = usenetProvider
	e.Providers = map[string]*storage.ProviderEntry{usenetProvider: {Provider: usenetProvider, ID: "nzo-1"}}
}

func TestPruneUsenetMissingMetaCountsAsDeleted(t *testing.T) {
	t.Parallel()
	missing := func(string) error {
		return errors.Join(errors.New("failed to get NZB"), customerror.ErrUsenetManifestMissing)
	}
	cases := []struct {
		name    string
		remove  func(string) error
		deleted int
	}{
		{"missing meta", missing, 1},
		{"other error", func(string) error { return errors.New("disk on fire") }, 0},
		{"fs not exist alone is not proof", func(string) error { return fs.ErrNotExist }, 0},
		{"success", func(string) error { return nil }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newPruneHarness(t, config.PruneModeDelete)
			h.addEntry(t, "aaaa", usenetEntry)
			var got []string
			h.m.removeUsenet = func(id string) error { got = append(got, id); return tc.remove(id) }

			deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))

			if deleted != tc.deleted || failed != 1-tc.deleted || h.has("aaaa") != (tc.deleted == 0) {
				t.Fatalf("deleted=%d failed=%d", deleted, failed)
			}
			if len(got) != 1 || got[0] != "nzo-1" {
				t.Fatalf("usenet calls %v", got)
			}
		})
	}
}

func TestPruneUsenetNotConfiguredKeepsEntry(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", usenetEntry)

	deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))

	if deleted != 0 || failed != 1 || !h.has("aaaa") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneConcurrentDeleteIsSkipped(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	cs := h.cands(t, "aaaa")
	if err := h.m.storage.Delete("aaaa"); err != nil {
		t.Fatal(err)
	}

	deleted, failed, _ := h.m.pruneEntries(context.Background(), cs)

	if deleted != 0 || failed != 0 || h.ledgerHas(t, "aaaa") || len(h.provider.deletedIDs()) != 0 {
		t.Fatalf("deleted=%d failed=%d ledger=%v", deleted, failed, h.ledger(t))
	}
}

func TestPruneNowExcludedIsSkipped(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	e := h.addEntry(t, "aaaa", nil)
	h.addEntry(t, "bbbb", nil)
	h.addEntry(t, "cccc", nil)
	cs := h.cands(t, "aaaa", "bbbb", "cccc")
	e.Manual = true
	if err := h.m.storage.AddOrUpdate(e); err != nil {
		t.Fatal(err)
	}
	if err := h.m.queue.Add(&storage.Entry{InfoHash: "bbbb", Name: "q"}); err != nil {
		t.Fatal(err)
	}

	deleted, failed, _ := h.m.pruneEntries(context.Background(), cs)

	if deleted != 1 || failed != 0 || !h.has("aaaa") || !h.has("bbbb") || h.has("cccc") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneStrictHoldersAbortOnUndecodable(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	cs := h.cands(t, "aaaa")
	h.corruptStore(t, "entries.db", "junk", []byte{0x0a, 0x05, 0x01})

	deleted, failed, _ := h.m.pruneEntries(context.Background(), cs)

	if deleted != 0 || failed != 1 || !h.has("aaaa") || len(h.provider.deletedIDs()) != 0 {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneStorageDeleteFailureStopsBatch(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	h.addEntry(t, "bbbb", nil)
	cs := h.cands(t, "aaaa", "bbbb")
	h.provider.onDelete = func(string) { _ = h.m.storage.Close() }
	cm := h.mount2(t)

	deleted, failed, _ := h.m.pruneEntries(context.Background(), cs)

	if deleted != 0 || failed != 1 || len(h.provider.deletedIDs()) != 1 || cm.count() != 0 {
		t.Fatalf("deleted=%d failed=%d provider=%v", deleted, failed, h.provider.deletedIDs())
	}
	logs := h.logs.String()
	if !strings.Contains(logs, `"level":"error"`) || !strings.Contains(logs, `"infohash":"aaaa"`) {
		t.Fatalf("no error log with infohash: %s", h.logs.String())
	}
}

func TestPruneOneMountRefreshPerBatch(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	cm := h.mount2(t)
	for _, hash := range []string{"aaaa", "bbbb", "cccc"} {
		h.addEntry(t, hash, nil)
	}

	deleted, _, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa", "bbbb", "cccc"))
	if deleted != 3 || cm.count() != 1 {
		t.Fatalf("deleted=%d refreshes=%d", deleted, cm.count())
	}
	h.provider.deleteErr = func(string) error { return errors.New("500") }
	h.provider.lookup = func(id string) (*types.Torrent, error) { return &types.Torrent{ID: id}, nil }
	h.addEntry(t, "dddd", nil)
	if deleted, _, _ = h.m.pruneEntries(context.Background(), h.cands(t, "dddd")); deleted != 0 || cm.count() != 1 {
		t.Fatalf("deleted=%d refreshes=%d", deleted, cm.count())
	}
}

func TestPruneCtxCancelStopsBatch(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	h.addEntry(t, "bbbb", nil)
	cs := h.cands(t, "aaaa", "bbbb")
	ctx, cancel := context.WithCancel(context.Background())
	h.provider.onDelete = func(string) { cancel() }

	deleted, failed, _ := h.m.pruneEntries(ctx, cs)

	if deleted != 1 || failed != 0 || h.has("aaaa") || !h.has("bbbb") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneHonoursMaxPerRunThroughRunPrune(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.reference(t, h.addEntry(t, "keep", nil))
	for _, hash := range []string{"e1", "e2", "e3", "e4", "e5"} {
		h.addEntry(t, hash, nil)
	}
	h.setPrune(t, func(p *config.Prune) { p.MaxPerRun = 2 })
	h.run(t)
	h.age(t, pruneGrace)

	r := h.run(t)

	if r.Deleted != 2 || r.Failed != 0 || r.BytesFreed != 200 || len(h.provider.deletedIDs()) != 2 {
		t.Fatalf("report %+v calls %v", r, h.provider.deletedIDs())
	}
}

func TestRefreshAndPruneDeleteAreExclusive(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, nil)
	lock := m.providerLock("rd")
	lock.Lock()
	client := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}
	done := make(chan error, 1)
	go func() { done <- m.doRefreshTorrents(context.Background(), "rd", client) }()

	select {
	case err := <-done:
		t.Fatalf("refresh ran while the provider lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	lock.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not resume after the lock was released")
	}
}

func TestPruneDeletesWhileRefreshIsHeldOff(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	cs := h.cands(t, "aaaa")
	lock := h.m.providerLock("torbox")
	lock.Lock()
	done := make(chan struct{})
	go func() {
		h.m.pruneEntries(context.Background(), cs)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("prune removed a placement while the provider lock was held")
	case <-time.After(100 * time.Millisecond):
	}
	lock.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prune did not resume")
	}
}

func TestPruneEndToEndDeletesOnlyUnreferencedNonManual(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.reference(t, h.addEntry(t, "refd", nil))
	h.addEntry(t, "gone", nil)
	h.addEntry(t, "hand", func(e *storage.Entry) { e.Manual = true })
	if err := os.MkdirAll(h.mount, 0o750); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	h.age(t, pruneGrace)

	r := h.run(t)

	if r.Deleted != 1 || r.Failed != 0 || r.BytesFreed != 100 {
		t.Fatalf("report %+v", r)
	}
	if h.has("gone") || !h.has("refd") || !h.has("hand") {
		t.Fatal("wrong entries deleted")
	}
	if ids := h.provider.deletedIDs(); len(ids) != 1 || ids[0] != "id-gone" {
		t.Fatalf("provider calls %v", ids)
	}
	if h.ledgerHas(t, "gone") {
		t.Fatal("ledger key of deleted entry survived")
	}
}

func TestPruneUnplacedEntryIsDeletedWithoutProviderCall(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", shareID(""))

	deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))

	if deleted != 1 || failed != 0 || len(h.provider.deletedIDs()) != 0 {
		t.Fatalf("deleted=%d failed=%d calls=%v", deleted, failed, h.provider.deletedIDs())
	}
}

func TestPruneBoundEntryIsExcludedAndNotStamped(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeReport)
	h.reference(t, h.addEntry(t, "keep", nil))
	e := h.addEntry(t, "bbbb", nil)
	h.fake.binding = reacquire.Binding{EntryID: "bbbb", EntryFileID: e.Files["a.mkv"].ID}

	r := h.run(t)

	if candidate(t, r, "bbbb").Excluded != "bound" || h.ledgerHas(t, "bbbb") {
		t.Fatalf("candidates %+v ledger %v", r.Candidates, h.ledger(t))
	}
}

func TestPruneBindingAppearingBeforeDeleteIsSkipped(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	e := h.addEntry(t, "aaaa", nil)
	cs := h.cands(t, "aaaa")
	h.fake.binding = reacquire.Binding{EntryID: "aaaa", EntryFileID: e.Files["a.mkv"].ID}

	deleted, failed, _ := h.m.pruneEntries(context.Background(), cs)

	if deleted != 0 || failed != 0 || !h.has("aaaa") || len(h.provider.deletedIDs()) != 0 {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneCoHolderAddedAfterSnapshotStopsProviderDelete(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", shareID("42"))
	cs := h.cands(t, "aaaa")
	h.addEntry(t, "late", shareID("42"))

	outcome, size := h.m.pruneOne(cs[0])

	if outcome != pruneDeleted || size != 0 || h.has("aaaa") || !h.has("late") || len(h.provider.deletedIDs()) != 0 {
		t.Fatalf("outcome=%v size=%d calls=%v", outcome, size, h.provider.deletedIDs())
	}
}

func TestPruneStorageDeleteNotFoundIsSkippedAndBatchContinues(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	h.addEntry(t, "bbbb", nil)
	cs := h.cands(t, "aaaa", "bbbb")
	h.provider.onDelete = func(id string) {
		if id == "id-aaaa" {
			_ = h.m.storage.Delete("aaaa")
		}
	}

	deleted, failed, _ := h.m.pruneEntries(context.Background(), cs)

	if deleted != 1 || failed != 0 || h.has("bbbb") || h.ledgerHas(t, "aaaa") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
	if strings.Contains(h.logs.String(), `"level":"error"`) {
		t.Fatalf("error logged: %s", h.logs.String())
	}
}

func TestPruneFailingEntriesDoNotStarveNewerOnes(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.reference(t, h.addEntry(t, "keep", nil))
	for _, hash := range []string{"f1", "f2", "g"} {
		h.addEntry(t, hash, nil)
	}
	for hash, ago := range map[string]time.Duration{"f1": 10 * 24 * time.Hour, "f2": 10 * 24 * time.Hour, "g": 8 * 24 * time.Hour} {
		if err := h.m.storage.SetUnreferencedSince(hash, h.now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.m.storage.SetLastReferencedCount(1); err != nil {
		t.Fatal(err)
	}
	if err := h.m.storage.SetLastScan(h.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.setPrune(t, func(p *config.Prune) { p.MaxPerRun = 2 })
	h.provider.deleteErr = func(id string) error {
		if id == "id-g" {
			return nil
		}
		return errors.New("500")
	}
	h.provider.lookup = func(id string) (*types.Torrent, error) { return &types.Torrent{ID: id}, nil }

	first := h.run(t)
	h.now = h.now.Add(time.Hour)
	second := h.run(t)

	if first.Failed != 2 || first.Deleted != 0 || second.Deleted != 1 || h.has("g") {
		t.Fatalf("first %+v second %+v", first, second)
	}
}

func TestPruneReacquireJobCreatedMidBatchSkipsEntry(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", nil)
	h.addEntry(t, "bbbb", nil)
	cs := h.cands(t, "aaaa", "bbbb")
	h.provider.onDelete = func(string) {
		h.fake.jobs = append(h.fake.jobs, reacquire.Job{EntryID: "bbbb", Status: reacquire.StatusSearching})
	}

	deleted, failed, _ := h.m.pruneEntries(context.Background(), cs)

	if deleted != 1 || failed != 0 || h.has("aaaa") || !h.has("bbbb") {
		t.Fatalf("deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneCancelDuringDeleteReturnsErrorWithoutSummary(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeReport)
	h.reference(t, h.addEntry(t, "keep", nil))
	h.addEntry(t, "aaaa", nil)
	h.run(t)
	h.age(t, pruneGrace)
	h.setPrune(t, func(p *config.Prune) { p.Mode = config.PruneModeDelete })
	before, prior := strings.Count(h.logs.String(), "prune finished"), h.m.LastPruneReport()
	ctx, cancel := context.WithCancel(context.Background())
	h.provider.onDelete = func(string) { cancel() }

	_, err := h.m.RunPrune(ctx)

	if !errors.Is(err, context.Canceled) || strings.Count(h.logs.String(), "prune finished") != before {
		t.Fatalf("err=%v logs=%s", err, h.logs.String())
	}
	if !h.m.LastPruneReport().StartedAt.Equal(prior.StartedAt) {
		t.Fatalf("cancelled run was recorded: %+v", h.m.LastPruneReport())
	}
}

func TestPruneMultiProviderPartialFailureRetriesViaNotFound(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "aaaa", func(e *storage.Entry) {
		e.Providers["rd"] = &storage.ProviderEntry{Provider: "rd", ID: "rd-1"}
	})
	rd := &fakePruneProvider{
		deleteErr: func(string) error { return errors.New("500") },
		lookup:    func(id string) (*types.Torrent, error) { return &types.Torrent{ID: id}, nil },
	}
	h.m.clients.Store("rd", rd)

	deleted, failed, _ := h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))
	if deleted != 0 || failed != 1 || !h.has("aaaa") {
		t.Fatalf("first run deleted=%d failed=%d", deleted, failed)
	}
	rd.deleteErr, rd.lookup = nil, nil
	h.provider.deleteErr = func(string) error { return errors.New("404") }

	deleted, failed, _ = h.m.pruneEntries(context.Background(), h.cands(t, "aaaa"))
	if deleted != 1 || failed != 0 || h.has("aaaa") {
		t.Fatalf("second run deleted=%d failed=%d", deleted, failed)
	}
}

func TestPruneSplitFamilySharingOnePlacementCallsProviderOnce(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "par", shareID("42"))
	h.addEntry(t, "ch1", shareID("42"))
	h.addEntry(t, "ch2", shareID("42"))

	deleted, failed, freed := h.m.pruneEntries(context.Background(), h.cands(t, "par", "ch1", "ch2"))

	if deleted != 3 || failed != 0 || freed != 100 || len(h.provider.deletedIDs()) != 1 {
		t.Fatalf("deleted=%d failed=%d freed=%d calls=%v", deleted, failed, freed, h.provider.deletedIDs())
	}
	if h.has("par") || h.has("ch1") || h.has("ch2") {
		t.Fatal("family member survived")
	}
}

func TestPruneFailureCountDroppedOnceEntryLeavesCandidates(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.reference(t, h.addEntry(t, "keep", nil))
	e := h.addEntry(t, "bbbb", nil)
	h.run(t)
	h.age(t, pruneGrace)
	h.provider.deleteErr = func(string) error { return errors.New("500") }
	h.provider.lookup = func(id string) (*types.Torrent, error) { return &types.Torrent{ID: id}, nil }
	if r := h.run(t); r.Failed != 1 || h.m.pruneFails["bbbb"] != 1 {
		t.Fatalf("report %+v fails %v", r, h.m.pruneFails)
	}
	h.reference(t, e)

	h.run(t)

	if _, ok := h.m.pruneFails["bbbb"]; ok {
		t.Fatalf("stale failure count: %v", h.m.pruneFails)
	}
}
