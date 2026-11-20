package manager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

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

const seasonTestPackHash = "0123456789abcdef0123456789abcdef01234567"

func seasonTestPack(savePath, category string) *storage.Entry {
	return &storage.Entry{
		InfoHash: seasonTestPackHash,
		Name:     "Show.S01-S02",
		Category: category,
		SavePath: savePath,
		Magnet:   "magnet:?xt=urn:btih:" + seasonTestPackHash + "&dn=Show.S01-S02",
	}
}

func seasonTestInfos() []SeasonInfo {
	return []SeasonInfo{
		{SeasonNumber: 1, InfoHash: "s1", Name: "Show.S01"},
		{SeasonNumber: 2, InfoHash: "s2", Name: "Show.S02"},
	}
}

func TestSeasonsGetSiblingFolders(t *testing.T) {
	t.Parallel()
	naming := config.WebDavUseArrSubmittedName
	root := t.TempDir()
	for name, tc := range map[string]struct {
		save string
		want []string
	}{
		"absolute": {
			save: filepath.Join(root, "sonarr", seasonTestPackHash),
			want: []string{filepath.Join(root, "sonarr", "s1"), filepath.Join(root, "sonarr", "s2")},
		},
		"absolute with a trailing separator": {
			save: filepath.Join(root, "sonarr", seasonTestPackHash) + string(filepath.Separator),
			want: []string{filepath.Join(root, "sonarr", "s1"), filepath.Join(root, "sonarr", "s2")},
		},
		"relative": {
			save: filepath.Join("sonarr", seasonTestPackHash),
			want: []string{filepath.Join("sonarr", "s1"), filepath.Join("sonarr", "s2")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pack := seasonTestPack(tc.save, "sonarr")
			seasons := convertToMultiSeason(pack, seasonTestInfos())
			for i, s := range seasons {
				if s.SavePath != tc.want[i] || !s.OwnsSavePath() {
					t.Fatalf(
						"season %d SavePath = %q (owned %v), want %q owned",
						i,
						s.SavePath,
						s.OwnsSavePath(),
						tc.want[i],
					)
				}
			}
			requireDisjoint(
				t,
				pack.DownloadPath(naming),
				seasons[0].DownloadPath(naming),
				seasons[1].DownloadPath(naming),
			)
		})
	}
}

func TestDeletingOneSeasonKeepsTheOtherSeasonsFiles(t *testing.T) {
	t.Parallel()
	naming := config.WebDavUseArrSubmittedName
	pack := seasonTestPack(filepath.Join(t.TempDir(), "sonarr", seasonTestPackHash), "sonarr")
	seasons := convertToMultiSeason(pack, seasonTestInfos())
	var files []string
	for _, s := range seasons {
		dir := s.DownloadPath(naming)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "ep.mkv")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	q := newQueue(nil, func() config.WebDavFolderNaming { return naming }, &grabIndex{}, zerolog.Nop())
	if err := q.deleteEntryFiles(seasons[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Fatalf("season 1's file survived its delete: %v", err)
	}
	if _, err := os.Stat(files[1]); err != nil {
		t.Fatalf("season 2's file went with season 1: %v", err)
	}
}

func TestLegacyPackSeasonsKeepThePacksFolder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, pack := range map[string]*storage.Entry{
		"category dir":   seasonTestPack(filepath.Join(root, "sonarr"), "sonarr"),
		"empty category": seasonTestPack(filepath.Join(root, "pack"), ""),
	} {
		for i, s := range convertToMultiSeason(pack, seasonTestInfos()) {
			if s.SavePath != pack.SavePath {
				t.Fatalf("%s: season %d SavePath = %q, want the pack's %q", name, i, s.SavePath, pack.SavePath)
			}
		}
	}
}

func requireDisjoint(t *testing.T, paths ...string) {
	t.Helper()
	for i, a := range paths {
		for j, b := range paths {
			if i != j && (a == b || nestedIn(a, b)) {
				t.Fatalf("%q and %q share a folder", a, b)
			}
		}
	}
}
