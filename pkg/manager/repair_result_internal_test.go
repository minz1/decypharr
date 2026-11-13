package manager

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const resultHash = "0123456789012345678901234567890123456789"

func resultPlacement(status types.TorrentStatus) *types.Torrent {
	return &types.Torrent{
		ID: "new", Debrid: "remaining", Status: status,
		Files: map[string]types.File{"video.mkv": {Name: "video.mkv", ID: "file-new"}},
	}
}

func resultEntry() *storage.Entry {
	stamp := time.Now()
	return &storage.Entry{
		InfoHash: resultHash, Name: "release", Protocol: config.ProtocolTorrent, ActiveProvider: "removed", Bad: true,
		Files: map[string]*storage.File{"video.mkv": {Name: "video.mkv", Deleted: true}},
		Providers: map[string]*storage.ProviderEntry{
			"removed": {Provider: "removed", ID: "old", Status: types.TorrentStatusDownloaded, DownloadedAt: &stamp},
		},
	}
}

type applyRepairCase struct {
	fresh   func() *storage.Entry
	result  RepairResult
	wantErr error
	check   func(t *testing.T, out *storage.Entry)
}

func applyRepairCases(stamp time.Time) map[string]applyRepairCase {
	return map[string]applyRepairCase{
		"healed adds the placement and activates it": {
			fresh:  resultEntry,
			result: RepairResult{Provider: "remaining", Placement: resultPlacement(types.TorrentStatusDownloaded)},
			check: func(t *testing.T, out *storage.Entry) {
				if out.ActiveProvider != "remaining" || out.Bad || out.Providers["remaining"].ID != "new" {
					t.Fatalf("merged = %+v", out)
				}
				if out.Files["video.mkv"].Deleted {
					t.Fatal("the placement's file stayed deleted")
				}
				if out.Providers["removed"].ID != "old" {
					t.Fatal("the old placement changed")
				}
			},
		},
		"healed with an existing placement activates it": {
			fresh: func() *storage.Entry {
				e := resultEntry()
				e.Providers["remaining"] = &storage.ProviderEntry{
					Provider: "remaining", ID: "kept", Status: types.TorrentStatusDownloaded, DownloadedAt: &stamp,
				}
				return e
			},
			result: RepairResult{Provider: "remaining", SeenID: "kept"},
			check: func(t *testing.T, out *storage.Entry) {
				if out.ActiveProvider != "remaining" || out.Bad || out.Providers["remaining"].ID != "kept" {
					t.Fatalf("merged = %+v", out)
				}
			},
		},
		"an activation that fails keeps the mark": {
			fresh:   resultEntry,
			result:  RepairResult{Provider: "remaining", Placement: resultPlacement(types.TorrentStatusDownloading)},
			wantErr: errActivateFailed,
		},
		"an existing placement that is no longer complete fails": {
			fresh: func() *storage.Entry {
				e := resultEntry()
				e.Providers["remaining"] = &storage.ProviderEntry{
					Provider: "remaining", ID: "kept", Status: types.TorrentStatusDownloading,
				}
				return e
			},
			result:  RepairResult{Provider: "remaining", SeenID: "kept"},
			wantErr: errActivateFailed,
		},
		"a missing entry changes nothing": {
			fresh:   func() *storage.Entry { return nil },
			result:  RepairResult{Provider: "remaining", Placement: resultPlacement(types.TorrentStatusDownloaded)},
			wantErr: storage.ErrNoChange,
		},
		"a newer placement on another provider survives": {
			fresh: func() *storage.Entry {
				e := resultEntry()
				e.Providers["other"] = &storage.ProviderEntry{
					Provider: "other", ID: "newer", Status: types.TorrentStatusDownloaded, DownloadedAt: &stamp,
				}
				e.Files["extra.mkv"] = &storage.File{Name: "extra.mkv"}
				return e
			},
			result: RepairResult{Provider: "remaining", Placement: resultPlacement(types.TorrentStatusDownloaded)},
			check: func(t *testing.T, out *storage.Entry) {
				if out.Providers["other"] == nil || out.Providers["other"].ID != "newer" ||
					out.Files["extra.mkv"] == nil {
					t.Fatalf("merged = %+v, want the newer placement and file kept", out)
				}
				if out.ActiveProvider != "remaining" {
					t.Fatalf("active = %q", out.ActiveProvider)
				}
			},
		},
		"a placement a sync recorded on the same provider is kept": {
			fresh: func() *storage.Entry {
				e := resultEntry()
				e.Providers["remaining"] = &storage.ProviderEntry{
					Provider: "remaining", ID: "synced", Status: types.TorrentStatusDownloaded, DownloadedAt: &stamp,
				}
				return e
			},
			result: RepairResult{
				Provider:  "remaining",
				SeenID:    "seen",
				Placement: resultPlacement(types.TorrentStatusDownloaded),
			},
			wantErr: errPlacementChanged,
		},
	}
}

func TestApplyRepair(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for name, tc := range applyRepairCases(now) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runApplyRepairCase(t, tc, now)
		})
	}
}

func runApplyRepairCase(t *testing.T, tc applyRepairCase, now time.Time) {
	fresh := tc.fresh()
	var before *storage.Entry
	if fresh != nil {
		before = fresh.Clone()
	}
	out, err := applyRepair(fresh, tc.result, now)
	if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
		t.Fatalf("error = %v, want %v", err, tc.wantErr)
	}
	if !reflect.DeepEqual(fresh, before) {
		t.Fatalf("fresh was written: %+v, want %+v", fresh, before)
	}
	if tc.check != nil {
		tc.check(t, out)
	}
}

type recordingProvider struct {
	replacementProvider

	name string
	mu   sync.Mutex
	ids  []string

	onDelete func()
}

func (c *recordingProvider) Config() config.Debrid {
	return config.Debrid{Name: c.name, Provider: "torbox"}
}

func (c *recordingProvider) DeleteTorrent(_ context.Context, id string) error {
	if c.onDelete != nil {
		c.onDelete()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
	return nil
}

func (c *recordingProvider) deleted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ids)
}

func newResultManager(t *testing.T, clients ...*recordingProvider) *Manager {
	t.Helper()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := withTestConfig(t, &Manager{
		storage: store, clients: xsync.NewMap[string, debrid.Client](), logger: zerolog.Nop(),
		entry: newEntryCache(func(string) bool { return false }, nil, nil),
	})
	for _, c := range clients {
		m.clients.Store(c.name, c)
	}
	m.fixer = NewFixer(m)
	return m
}

func storeResultEntry(t *testing.T, m *Manager, edit func(*storage.Entry)) {
	t.Helper()
	e := resultEntry()
	if edit != nil {
		edit(e)
	}
	if err := m.storage.AddOrUpdate(e); err != nil {
		t.Fatal(err)
	}
}

type releaseCase struct {
	store       bool
	edit        func(*storage.Entry)
	result      RepairResult
	wantErr     error
	wantDeleted []string
	wantActive  string
}

func releaseCases() map[string]releaseCase {
	healed := RepairResult{
		Provider: "remaining", SeenID: "old", Placement: resultPlacement(types.TorrentStatusDownloaded),
		Release: &placementRelease{provider: "remaining", id: "old", grab: resultHash, delete: true, library: true},
	}
	failing := RepairResult{
		Provider: "remaining", SeenID: "old", Placement: resultPlacement(types.TorrentStatusDownloading),
		Release: healed.Release,
	}
	return map[string]releaseCase{
		"healed releases the old placement": {
			store: true, result: healed, wantDeleted: []string{"old"}, wantActive: "remaining",
			edit: func(e *storage.Entry) {
				e.ActiveProvider = "remaining"
				e.Providers["remaining"] = &storage.ProviderEntry{
					Provider: "remaining", ID: "old", Status: types.TorrentStatusDownloaded,
				}
			},
		},
		"a failed activation discards the new placement and keeps the old": {
			store:       true,
			result:      failing,
			wantErr:     errActivateFailed,
			wantDeleted: []string{"new"},
			wantActive:  "remaining",
			edit: func(e *storage.Entry) {
				e.ActiveProvider = "remaining"
				e.Providers["remaining"] = &storage.ProviderEntry{
					Provider: "remaining", ID: "old", Status: types.TorrentStatusDownloaded,
				}
			},
		},
		"a deleted entry discards the new placement": {
			result: healed, wantErr: errRepairSuperseded, wantDeleted: []string{"new"},
		},
		"a placement recorded meanwhile is kept and releases nothing": {
			store:       true,
			result:      healed,
			wantErr:     errPlacementChanged,
			wantDeleted: []string{"new"},
			wantActive:  "remaining",
			edit: func(e *storage.Entry) {
				e.ActiveProvider = "remaining"
				e.Providers["remaining"] = &storage.ProviderEntry{
					Provider: "remaining", ID: "synced", Status: types.TorrentStatusDownloaded,
				}
			},
		},
		"a failed activation of an existing placement discards nothing": {
			store: true, result: RepairResult{Provider: "remaining", SeenID: "kept"},
			wantErr: errActivateFailed, wantActive: "removed",
			edit: func(e *storage.Entry) {
				e.Providers["remaining"] = &storage.ProviderEntry{
					Provider: "remaining", ID: "kept", Status: types.TorrentStatusDownloading,
				}
			},
		},
	}
}

func TestApplyRepairResultReleasesOrDiscards(t *testing.T) {
	t.Parallel()
	for name, tc := range releaseCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) { runReleaseCase(t, tc) })
		})
	}
}

func runReleaseCase(t *testing.T, tc releaseCase) {
	client := &recordingProvider{name: "remaining"}
	m := newResultManager(t, client)
	if tc.store {
		storeResultEntry(t, m, tc.edit)
	}
	err := m.applyRepairResult(t.Context(), resultHash, tc.result)
	if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
		t.Fatalf("error = %v, want %v", err, tc.wantErr)
	}
	drainPlacements(t, m)
	if got := client.deleted(); !slices.Equal(got, tc.wantDeleted) {
		t.Fatalf("deleted = %v, want %v", got, tc.wantDeleted)
	}
	if tc.wantActive == "" {
		return
	}
	stored, getErr := m.storage.Get(resultHash)
	if getErr != nil || stored.ActiveProvider != tc.wantActive {
		t.Fatalf("stored = %+v, %v; want %s active", stored, getErr, tc.wantActive)
	}
}

func TestWithBadWritesOnlyTheFlag(t *testing.T) {
	t.Parallel()
	fresh := resultEntry()
	fresh.Files["extra.mkv"] = &storage.File{Name: "extra.mkv"}
	before := fresh.Clone()
	out, err := withBad(fresh, false, time.Now())
	if err != nil || out.Bad || !reflect.DeepEqual(out.Files, before.Files) ||
		!reflect.DeepEqual(out.Providers, before.Providers) {
		t.Fatalf("out = %+v, %v; want only the flag changed", out, err)
	}
	if !fresh.Bad {
		t.Fatal("withBad wrote its input")
	}
	if _, err = withBad(out, false, time.Now()); !errors.Is(err, storage.ErrNoChange) {
		t.Fatalf("error = %v, want ErrNoChange", err)
	}
}

// A sync that writes the stored entry while a repair is submitting must not be overwritten by the repair's result.
func TestFixTorrentKeepsWhatASyncWroteMeanwhile(t *testing.T) {
	t.Parallel()
	client := &healProvider{}
	var h *healHarness
	client.submit = func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
		stored, err := h.fixer.manager.storage.Get(h.entry.InfoHash)
		if err != nil {
			return nil, err
		}
		stored.Files["extra.mkv"] = &storage.File{Name: "extra.mkv", InfoHash: stored.InfoHash}
		stored.Providers["other"] = &storage.ProviderEntry{
			Provider: "other", ID: "newer", Status: types.TorrentStatusDownloaded,
		}
		if err = h.fixer.manager.storage.AddOrUpdate(stored); err != nil {
			return nil, err
		}
		return client.replacementProvider.SubmitMagnet(ctx, torrent)
	}
	h = newHealHarness(t, client)
	got := <-h.fixAsync(t.Context())
	if got.err != nil || got.result == nil || !got.result.Success {
		t.Fatalf("heal = %#v, error = %v; want success", got.result, got.err)
	}
	stored, err := h.fixer.manager.storage.Get(h.entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ActiveProvider != "remaining" || stored.Providers["remaining"] == nil {
		t.Fatalf("stored = %+v, want the new placement active", stored)
	}
	if stored.Files["extra.mkv"] == nil || stored.Providers["other"] == nil {
		t.Fatalf("stored = %+v, want the sync's file and placement kept", stored)
	}
}

// A switch applies its move onto the stored entry and then removes the source placement from the record; a file
// state written during the move survives, and the source torrent is deleted once no record lists it.
func TestSwitchKeepsWhatASyncWroteDuringTheMove(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		source := &recordingProvider{name: "removed"}
		target := &recordingProvider{name: "remaining"}
		m := newResultManager(t, source, target)
		stamp := time.Now()
		storeResultEntry(t, m, func(e *storage.Entry) {
			e.Bad = false
			e.Providers["removed"].DownloadedAt = &stamp
		})
		stored, err := m.storage.Get(resultHash)
		if err != nil {
			t.Fatal(err)
		}
		m.clients.Store("remaining", &healProvider{
			replacementProvider: replacementProvider{},
			submit: func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
				fresh, getErr := m.storage.Get(resultHash)
				if getErr != nil {
					return nil, getErr
				}
				fresh.Files["extra.mkv"] = &storage.File{Name: "extra.mkv", InfoHash: resultHash}
				if putErr := m.storage.AddOrUpdate(fresh); putErr != nil {
					return nil, putErr
				}
				return target.replacementProvider.SubmitMagnet(ctx, torrent)
			},
		})
		job := &storage.SwitcherJob{
			ID: "job", InfoHash: resultHash, SourceProvider: "removed", TargetProvider: "remaining",
			Status: storage.SwitcherStatusPending,
		}
		m.executeMigration(t.Context(), job, stored)
		if job.Status != storage.SwitcherStatusCompleted {
			t.Fatalf("job = %+v, want completed", job)
		}
		drainPlacements(t, m)
		after, err := m.storage.Get(resultHash)
		if err != nil {
			t.Fatal(err)
		}
		if after.ActiveProvider != "remaining" || after.Providers["removed"] != nil {
			t.Fatalf("stored = %+v, want remaining active and the source placement gone", after)
		}
		if after.Files["extra.mkv"] == nil {
			t.Fatalf("stored files = %+v, want the file written during the move", after.Files)
		}
		if got := source.deleted(); !slices.Equal(got, []string{"old"}) {
			t.Fatalf("source deleted = %v, want [old]", got)
		}
	})
}

func storeMoveAndApply(t *testing.T, m *Manager, fixer *Fixer, entry *storage.Entry) {
	t.Helper()
	if err := m.storage.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	result, err := fixer.MoveTorrent(entry, "remaining", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.applyRepairResult(t.Context(), entry.InfoHash, result); err != nil {
		t.Fatal(err)
	}
}
