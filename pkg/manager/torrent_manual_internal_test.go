package manager

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestRefreshKeepsManual(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, filepath.Join(t.TempDir(), "db"))
	m.clients.Store("rd", completedTorrentProvider{})
	const hash = "0123456789012345678901234567890123456789"
	if err := m.storage.AddOrUpdate(&storage.Entry{
		InfoHash: hash, Name: "pack", Protocol: config.ProtocolTorrent, Manual: true,
		Files: map[string]*storage.File{}, Providers: map[string]*storage.ProviderEntry{},
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := m.processSyncTorrent(t.Context(), &types.Torrent{
		InfoHash: hash, Name: "pack", Debrid: "rd", ID: "id1", Status: types.TorrentStatusDownloaded,
		Files: map[string]types.File{"a.mkv": {Name: "a.mkv", Size: 1, Link: "https://x/a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Manual {
		t.Fatal("refresh dropped Manual")
	}
}

// Providers report 0–100; a synced entry and its placement store the 0..1 fraction the rest of the manager uses.
func TestSyncStoresProgressAsFraction(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, filepath.Join(t.TempDir(), "db"))
	m.clients.Store("rd", completedTorrentProvider{})
	entry, err := m.processSyncTorrent(t.Context(), &types.Torrent{
		InfoHash: "0123456789012345678901234567890123456789", Name: "pack", Debrid: "rd", ID: "id1",
		Status: types.TorrentStatusDownloading, Progress: 40,
		Files: map[string]types.File{"a.mkv": {Name: "a.mkv", Size: 1, Link: "https://x/a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if placement := entry.Providers["rd"]; entry.Progress != 0.4 || placement.Progress != 0.4 {
		t.Fatalf("entry progress %v, placement progress %v, want 0.4", entry.Progress, placement.Progress)
	}
}

func TestSyncHealsALegacyDanglingActive(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, filepath.Join(t.TempDir(), "db"))
	stamp := time.Now()
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789", Name: "pack", ActiveProvider: "gone", IsComplete: true,
		Providers: map[string]*storage.ProviderEntry{
			"a": {Provider: "a", ID: "1", Status: types.TorrentStatusDownloaded, DownloadedAt: &stamp},
			"b": {Provider: "b", ID: "2", Status: types.TorrentStatusDownloaded, DownloadedAt: &stamp},
		},
	}
	healDanglingActive(entry, []string{"b", "a"})
	if entry.ActiveProvider != "b" {
		t.Fatalf("active = %q, want b", entry.ActiveProvider)
	}
	if err := m.storage.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	if n := m.storage.InvariantViolations(); n != 0 {
		t.Fatalf("violations = %d", n)
	}
}
