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
