package manager

import (
	"path/filepath"
	"testing"

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
	entry, err := m.processSyncTorrent(&types.Torrent{
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
