package storage

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

const folderTestHash = "0123456789abcdef0123456789abcdef01234567"

func TestOwnsSavePath(t *testing.T) {
	t.Parallel()
	h := folderTestHash
	j := filepath.Join
	sep := string(filepath.Separator)
	root := j(sep, "dl")
	cases := []struct {
		name     string
		hash     string
		savePath string
		category string
		want     bool
	}{
		{"new layout", h, j(root, "sonarr", h), "sonarr", true},
		{"new layout, trailing slash", h, j(root, "sonarr", h) + sep, "sonarr", true},
		{"recategorized new record", h, j(root, "sonarr", h), "sonarr-imported", true},
		{"relative new layout", h, j("sonarr", h), "sonarr", true},
		{"legacy", h, j(root, "sonarr"), "sonarr", false},
		{"legacy, category is the id", h, j(root, h), h, false},
		{"legacy, category x/H", h, j(root, "x", h), "x/" + h, false},
		{"legacy, category H/", h, j(root, h), h + "/", false},
		{"legacy, category /H", h, j(root, h), "/" + h, false},
		{"category dot", h, j(root, h), ".", false},
		{"category empty", h, j(root, h), "", false},
		{"category cleans to the id", h, j(root, h), "a/../" + h, false},
		{"category keeps dotdot", h, j(sep, h), "../" + h, false},
		{"category slash, manual root named H", h, j(sep, "data", h), "/", false},
		{"category slash dotdot", h, j(root, h), "/..", false},
		{"category /H/..", h, j(root, h), "/" + h + "/..", false},
		{"relative save path shorter than category", h, h, "a/b/c", true},
		{"string suffix, not element suffix", h, j(root, "sonar", h), "r/" + h, true},
		{"relative legacy, same length as category", h, h, h, false},
		{"legacy season, pack category is the season id", "s1", j(root, "s1"), "s1", false},
		{"empty infohash", "", j(root, "sonarr", "_"), "sonarr", false},
		{"empty save path", h, "", "sonarr", false},
		{"save path with trailing dot, new layout", h, j(root, "sonarr", h) + sep + ".", "sonarr", true},
		{"legacy save path with trailing slash", h, j(root, h) + sep, h, false},
		{"legacy save path with trailing dot", h, j(root, h) + sep + ".", h, false},
		{"category dot slash H", h, j(root, h), "./" + h, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Entry{InfoHash: tc.hash, SavePath: tc.savePath, Category: tc.category}
			if got := e.OwnsSavePath(); got != tc.want {
				t.Fatalf("OwnsSavePath(save=%q, category=%q) = %v, want %v", tc.savePath, tc.category, got, tc.want)
			}
		})
	}
}

func TestOwnsSavePathNilReceiver(t *testing.T) {
	t.Parallel()
	var e *Entry
	if e.OwnsSavePath() {
		t.Fatal("a nil entry owns no folder")
	}
}

func TestOwnsSavePathRefusesWindowsLegacyShapes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "windows" {
		t.Skip("windows path shapes")
	}
	h := folderTestHash
	cases := []struct{ name, save, category string }{
		{"unc dotdot category", `C:\dl\` + h, `\\srv\share\..\..\` + h},
		{"volume category", `C:\dl\` + h, `C:\` + h},
		{"volume-relative save path", `C:` + h, h},
		{"backslash dotdot category", filepath.Join(`C:\incoming`, `..\`+h), `..\` + h},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Entry{InfoHash: h, SavePath: tc.save, Category: tc.category}
			if e.OwnsSavePath() {
				t.Fatalf("OwnsSavePath(save=%q, category=%q) = true", tc.save, tc.category)
			}
		})
	}
}

func TestEntrySavePath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(string(filepath.Separator), "dl", "sonarr")
	if got, want := EntrySavePath(dir, folderTestHash), filepath.Join(dir, folderTestHash); got != want {
		t.Fatalf("EntrySavePath = %q, want %q", got, want)
	}
	if got, want := EntrySavePath(dir, "a/b"), filepath.Join(dir, "a_b"); got != want {
		t.Fatalf("EntrySavePath = %q, want %q", got, want)
	}
	if got, want := EntrySavePath(dir, ".."), filepath.Join(dir, "_"); got != want {
		t.Fatalf("EntrySavePath = %q, want %q", got, want)
	}
}

func TestNewLayoutDownloadPathKeepsTheOldDerivation(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "sonarr")
	e := &Entry{
		InfoHash: folderTestHash,
		Name:     "Show.S01E01.mkv",
		Magnet:   "magnet:?xt=urn:btih:" + folderTestHash + "&dn=Show.Release.Name",
		SavePath: EntrySavePath(dir, folderTestHash),
	}
	for _, tc := range []struct {
		naming config.WebDavFolderNaming
		folder string
	}{
		{config.WebDavUseArrSubmittedName, "Show.Release.Name"},
		{config.WebDavUseFileName, "Show.S01E01"},
	} {
		naming, folder := tc.naming, tc.folder
		e.ContentPath = e.DownloadPath(naming)
		data, err := encodeQueued(e, false)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeQueued(data, false)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, folderTestHash, folder)
		if got.SavePath != e.SavePath || got.ContentPath != want || got.DownloadPath(naming) != want {
			t.Fatalf("%s: SavePath %q ContentPath %q DownloadPath %q, want %q",
				naming, got.SavePath, got.ContentPath, got.DownloadPath(naming), want)
		}
	}
}
