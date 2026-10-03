package manager

import (
	"path/filepath"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
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

// A pack fanned out by an earlier version has season entries whose IDs came
// from another hash. Resuming it must reuse those entries, not add copies.
func TestSeasonFanOutReusesEarlierSeasonIDs(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, filepath.Join(t.TempDir(), "db"))
	pack := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789",
		Name:     "Show.S01-S02",
		Protocol: config.ProtocolTorrent,
		Files: map[string]*storage.File{
			"Show.S01E01.mkv": {Name: "Show.S01E01.mkv", Size: 5, InfoHash: "0123456789012345678901234567890123456789"},
			"Show.S02E01.mkv": {Name: "Show.S02E01.mkv", Size: 5, InfoHash: "0123456789012345678901234567890123456789"},
		},
		Providers: map[string]*storage.ProviderEntry{},
	}
	found, seasons := m.downloader.detectMultiSeason(pack)
	if !found {
		t.Fatal("season pack was not detected")
	}
	results := convertToMultiSeason(pack, seasons)
	const legacyID = "0f1e2d3c4b5a69788796a5b4c3d2e1f0" // an md5-era season ID
	var legacyName string
	for _, season := range results {
		if season.Files["Show.S01E01.mkv"] != nil {
			legacy := *season
			legacy.InfoHash = legacyID
			legacyName = legacy.Name
			if err := m.queue.Add(&legacy); err != nil {
				t.Fatal(err)
			}
		}
	}

	results = convertToMultiSeason(pack, seasons)
	m.downloader.adoptEarlierSeasonIDs(pack, results)
	for _, season := range results {
		switch {
		case season.Name == legacyName && season.InfoHash != legacyID:
			t.Fatalf("season %q got ID %q, want the earlier %q", season.Name, season.InfoHash, legacyID)
		case season.Name != legacyName && season.InfoHash == legacyID:
			t.Fatalf("season %q took another season's ID", season.Name)
		}
	}
}
