package manager

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestSeasonParserPatterns(t *testing.T) {
	t.Parallel()
	sp := newSeasonParser()
	for name, want := range map[string]string{
		"Show.S01-S03.1080p.WEB-DL": "Show.S02.1080p.WEB-DL",
		"Show.S01-08.720p":          "Show.S02.720p",
		"Show Complete.Series":      "Show Season 02",
		"Show 1080p WEB-DL":         "Show S02 1080p WEB-DL",
		"Show S04 1080p":            "Show S04 1080p",
		"Show":                      "Show S02",
	} {
		if got := sp.replaceMultiSeasonPattern(name, 2); got != want {
			t.Errorf("replaceMultiSeasonPattern(%q) = %q, want %q", name, got, want)
		}
	}
	if got := sp.extractSeason("Show/Season 05/e01.mkv"); got != 5 {
		t.Errorf("extractSeason = %d, want 5", got)
	}
	if !sp.hasMultiSeasonIndicators("Show.Seasons 1-4") || sp.hasMultiSeasonIndicators("Show.S01E01") {
		t.Error("multi-season indicators misclassified")
	}
}

// Season IDs keep the 32-hex-digit shape persisted entries have, and stay
// stable for one pack and season.
func TestGenerateSeasonHashIsStable(t *testing.T) {
	t.Parallel()
	first := generateSeasonHash("0123456789012345678901234567890123456789", 1)
	if len(first) != 32 || first != generateSeasonHash("0123456789012345678901234567890123456789", 1) {
		t.Fatalf("season hash %q is not a stable 32-digit ID", first)
	}
	if first == generateSeasonHash("0123456789012345678901234567890123456789", 2) {
		t.Fatal("two seasons share an ID")
	}
}

// A resumed pack must find the season entries its earlier fan-out saved:
// every fan-out gives a season the ID derived from the pack and season
// number, so the second pass looks up the same keys instead of adding copies.
func TestSeasonFanOutIDsAreDerivedFromPackAndSeason(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, filepath.Join(t.TempDir(), "db"))
	const packHash = "0123456789012345678901234567890123456789"
	pack := &storage.Entry{
		InfoHash: packHash,
		Name:     "Show.S01-S02",
		Protocol: config.ProtocolTorrent,
		Files: map[string]*storage.File{
			"Show.S01E01.mkv": {Name: "Show.S01E01.mkv", Size: 5, InfoHash: packHash},
			"Show.S02E01.mkv": {Name: "Show.S02E01.mkv", Size: 5, InfoHash: packHash},
		},
		Providers: map[string]*storage.ProviderEntry{},
	}
	for pass := range 2 {
		found, seasons := m.downloader.detectMultiSeason(pack)
		if !found || len(seasons) != 2 {
			t.Fatalf("pass %d: detected %t with %d seasons, want 2", pass, found, len(seasons))
		}
		for _, season := range convertToMultiSeason(pack, seasons) {
			number := 1
			if season.Files["Show.S02E01.mkv"] != nil {
				number = 2
			}
			if want := generateSeasonHash(packHash, number); season.InfoHash != want {
				t.Fatalf("pass %d: season %d ID = %q, want %q", pass, number, season.InfoHash, want)
			}
		}
	}
}

func TestSeasonSplitChildrenInheritManual(t *testing.T) {
	t.Parallel()
	pack := &storage.Entry{InfoHash: "pack", Name: "Show.S01-S02", Manual: true}
	seasons := []SeasonInfo{
		{SeasonNumber: 1, InfoHash: "s1", Name: "Show.S01"},
		{SeasonNumber: 2, InfoHash: "s2", Name: "Show.S02"},
	}
	children := convertToMultiSeason(pack, seasons)
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}
	for _, child := range children {
		if !child.Manual {
			t.Fatalf("child %s lost Manual", child.InfoHash)
		}
	}
}

// Deleting a season pack mid fan-out left the in-flight season queued with no worker.
func TestDeletingSeasonPackRemovesInFlightSeason(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, entry, torrent, children := newSeasonPackTest(t, "Show.S01-S02")
		completed := seedCompletedSeason(t, m, entry)
		if mkdirErr := os.MkdirAll(completed.DownloadPath(""), 0o755); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		m.processNewTorrent(entry, torrent)
		synctest.Wait()
		for _, child := range children {
			if !m.queue.Contains(child.InfoHash) {
				t.Fatalf("season %s not queued before the delete", child.Name)
			}
		}
		if deleteErr := m.queue.Delete(entry.InfoHash, true, nil); deleteErr != nil {
			t.Fatal(deleteErr)
		}
		m.downloadTasks.Wait()
		synctest.Wait()
		for _, child := range children {
			checkSeasonAfterPackDelete(t, m, child, child.InfoHash == completed.InfoHash)
		}
	})
}

// checkSeasonAfterPackDelete checks a completed season kept its entry and folder and an in-flight one lost both.
func checkSeasonAfterPackDelete(t *testing.T, m *Manager, season *storage.Entry, completed bool) {
	t.Helper()
	saved, getErr := m.queue.GetTorrent(season.InfoHash)
	_, statErr := os.Stat(season.DownloadPath(""))
	if completed {
		if getErr != nil || !saved.IsComplete || statErr != nil {
			t.Errorf("completed season %s lost: entry %v, folder %v", season.Name, getErr, statErr)
		}
		return
	}
	if getErr == nil {
		t.Errorf("season %s still queued after its pack was deleted: %#v", season.Name, saved)
	}
	if !os.IsNotExist(statErr) {
		t.Errorf("season %s folder left behind: %v", season.Name, statErr)
	}
}

// A season sharing the pack's folder must not wipe it when the pack is deleted with its files kept.
func TestDeletingSeasonPackKeepsSharedFolder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m, entry, torrent, children := newSeasonPackTest(t, "Show.S01.1080p")
		folder := entry.DownloadPath("")
		for _, child := range children {
			if child.DownloadPath("") != folder {
				t.Fatalf("season %s folder %q, want the pack's %q", child.Name, child.DownloadPath(""), folder)
			}
		}
		kept := filepath.Join(folder, "imported.mkv")
		if mkdirErr := os.MkdirAll(folder, 0o755); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		if writeErr := os.WriteFile(kept, []byte("media"), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		m.processNewTorrent(entry, torrent)
		synctest.Wait()
		if deleteErr := m.queue.Delete(entry.InfoHash, false, nil); deleteErr != nil {
			t.Fatal(deleteErr)
		}
		m.downloadTasks.Wait()
		synctest.Wait()

		for _, child := range children {
			if m.queue.Contains(child.InfoHash) {
				t.Errorf("season %s still queued after its pack was deleted", child.Name)
			}
		}
		if _, statErr := os.Stat(kept); statErr != nil {
			t.Fatalf("shared folder lost a kept file: %v", statErr)
		}
	})
}

// newSeasonPackTest queues a finished two-season pack whose files never appear on the mount.
func newSeasonPackTest(t *testing.T, name string) (*Manager, *storage.Entry, *types.Torrent, []*storage.Entry) {
	t.Helper()
	m := newShutdownTestManager(t, t.TempDir())
	m.mountManager = shutdownMount{stop: func() error { return nil }}
	t.Cleanup(func() { _ = m.Stop() })
	entry := &storage.Entry{
		InfoHash:       "0123456789012345678901234567890123456789",
		Name:           name,
		Protocol:       config.ProtocolTorrent,
		State:          storage.EntryStateDownloading,
		ActiveProvider: "provider",
		Action:         config.DownloadActionSymlink,
		SavePath:       t.TempDir(),
		Files:          map[string]*storage.File{},
		Providers:      map[string]*storage.ProviderEntry{},
	}
	torrent := &types.Torrent{
		ID: "provider-id", Debrid: "provider", InfoHash: entry.InfoHash, Name: entry.Name,
		Status: types.TorrentStatusDownloaded, Progress: 100,
		Files: map[string]types.File{
			"Show.S01E01.mkv": {Name: "Show.S01E01.mkv", ID: "1", Size: 5},
			"Show.S02E01.mkv": {Name: "Show.S02E01.mkv", ID: "2", Size: 5},
		},
	}
	applyDebridTorrentToEntry(entry, torrent)
	m.clients.Store("provider", completedTorrentProvider{torrent: torrent})
	if addErr := m.queue.Add(entry); addErr != nil {
		t.Fatal(addErr)
	}
	if mkdirErr := os.MkdirAll(m.GetTorrentMountPath(entry), 0o755); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	_, seasons := m.downloader.detectMultiSeason(entry)
	children := convertToMultiSeason(entry, seasons)
	if len(children) != 2 {
		t.Fatalf("seasons = %d, want 2", len(children))
	}
	return m, entry, torrent, children
}
