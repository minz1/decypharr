package repair

import (
	"context"
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type removedProviderBackend struct {
	Backend

	reinsert func(*storage.Entry) error
}

func (b *removedProviderBackend) ProviderClient(string) debrid.Client { return nil }
func (b *removedProviderBackend) ReinsertEntry(_ context.Context, entry *storage.Entry) error {
	return b.reinsert(entry)
}

func TestProbeEntryWithRemovedProvider(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		autoRepair  bool
		replacement bool
		unrestrict  bool
	}{
		{name: "check only"},
		{name: "unrestrict", unrestrict: true},
		{name: "repair fails", autoRepair: true},
		{name: "activate replacement", autoRepair: true, replacement: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newRepairTestStorage(t)
			entry := &storage.Entry{
				InfoHash: "0123456789012345678901234567890123456789", Name: "release",
				Protocol: config.ProtocolTorrent, ActiveProvider: "removed",
				Files: map[string]*storage.File{
					"video.mkv": {Name: "video.mkv", InfoHash: "0123456789012345678901234567890123456789"},
				},
				Providers: map[string]*storage.ProviderEntry{
					"removed":   {Provider: "removed", ID: "old", Status: types.TorrentStatusDownloaded},
					"remaining": {Provider: "remaining", ID: "new", Status: types.TorrentStatusDownloaded},
				},
			}
			if err := store.AddOrUpdate(entry); err != nil {
				t.Fatal(err)
			}
			calls := 0
			backend := &removedProviderBackend{reinsert: func(e *storage.Entry) error {
				calls++
				if !tc.replacement {
					return errors.New("no replacement available")
				}
				if err := e.ActivatePlacement("remaining"); err != nil {
					return err
				}
				return store.AddOrUpdate(e)
			}}
			service := New(Dependencies{Storage: store, Backend: backend})
			candidate := &candidate{name: entry.Name, item: &storage.EntryItem{Name: entry.Name, Files: entry.Files}}
			health := service.probeEntry(
				t.Context(),
				"run",
				candidate,
				newErrorCache(),
				nil,
				RunOptions{UnrestrictLink: tc.unrestrict},
				tc.autoRepair,
			)
			if tc.autoRepair && calls != 1 || !tc.autoRepair && calls != 0 {
				t.Fatalf("repair calls = %d", calls)
			}
			assertRemovedProviderOutcome(t, store, entry, health, tc.replacement)
		})
	}
}

// assertRemovedProviderOutcome checks the probe result, and what was saved,
// for an entry whose active provider is gone: broken unless a replacement
// placement was activated.
func assertRemovedProviderOutcome(
	t *testing.T,
	store *storage.Storage,
	entry *storage.Entry,
	health *storage.EntryHealth,
	replacement bool,
) {
	t.Helper()
	want := storage.HealthBroken
	if replacement {
		want = storage.HealthHealthy
	}
	if health.Status != want {
		t.Fatalf("health = %s, want %s", health.Status, want)
	}
	if want == storage.HealthBroken &&
		(health.FailureReason != "provider_client_not_found" || health.BrokenCount != 1) {
		t.Fatalf("broken health = %#v", health)
	}
	saved, err := store.GetEntryHealth(entry.Name)
	if err != nil || saved.Status != want {
		t.Fatalf("saved health = %#v, error = %v", saved, err)
	}
	loaded, err := store.Get(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if replacement && loaded.ActiveProvider != "remaining" {
		t.Fatalf("active provider = %q", loaded.ActiveProvider)
	}
}
