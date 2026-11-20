package manager

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestImportRequestSkipMultiSeason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  ImportType
		skip bool
		want bool
	}{
		{"api default fans out", ImportTypeAPI, false, false},
		{"api explicit skip", ImportTypeAPI, true, true},
		{"qbit never fans out", ImportTypeQBit, false, true},
		{"sabnzbd never fans out", ImportTypeSABnzbd, false, true},
		{"watch never fans out", ImportTypeWatch, false, true},
		{"switcher never fans out", ImportSwitcher, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &ImportRequest{Type: tt.typ, SkipMultiSeason: tt.skip}
			if got := r.skipMultiSeason(); got != tt.want {
				t.Fatalf("skipMultiSeason() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewTorrentQueueEntryKeepsArrPacksWhole(t *testing.T) {
	t.Parallel()

	req := &ImportRequest{
		Type:   ImportTypeQBit,
		Magnet: &utils.Magnet{Name: "Show S01-S04", InfoHash: "abc"},
		Arr:    arr.Arr{Name: "sonarr"},
	}
	if entry := newTorrentQueueEntry(req, storage.EntryStatusQueued, ""); !entry.SkipMultiSeason {
		t.Fatal("qbit entry SkipMultiSeason = false, want true")
	}
}

func TestNewNZBQueueEntryKeepsArrPacksWhole(t *testing.T) {
	t.Parallel()

	req := &ImportRequest{ID: "nzb-1", Name: "Show S01-S04", Type: ImportTypeSABnzbd, Arr: arr.Arr{Name: "sonarr"}}
	if entry := newNZBQueueEntry(req, "/staged/nzb-1.nzb"); !entry.SkipMultiSeason {
		t.Fatal("sabnzbd entry SkipMultiSeason = false, want true")
	}
}

func TestQueueEntriesCarryManual(t *testing.T) {
	t.Parallel()

	magnet := &utils.Magnet{Name: "Show", InfoHash: "abc"}
	tests := []struct {
		name   string
		typ    ImportType
		manual bool
	}{
		{"api manual", ImportTypeAPI, true},
		{"qbit", ImportTypeQBit, false},
		{"sabnzbd", ImportTypeSABnzbd, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			torrentReq := &ImportRequest{Type: tt.typ, Manual: tt.manual, Magnet: magnet}
			torrentEntry := newTorrentQueueEntry(torrentReq, storage.EntryStatusDownloading, "")
			if torrentEntry.Manual != tt.manual {
				t.Fatalf("torrent entry Manual = %v, want %v", torrentEntry.Manual, tt.manual)
			}
			nzbReq := &ImportRequest{ID: "nzb-1", Name: "Show", Type: tt.typ, Manual: tt.manual}
			if got := newNZBQueueEntry(nzbReq, "/x").Manual; got != tt.manual {
				t.Fatalf("nzb entry Manual = %v, want %v", got, tt.manual)
			}
		})
	}
}

func nestedIn(parent, child string) bool {
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func TestQueueEntriesGetAFolderOfTheirOwn(t *testing.T) {
	t.Parallel()
	namings := []config.WebDavFolderNaming{config.WebDavUseArrSubmittedName, config.WebDavUseFileName}
	for _, naming := range namings {
		t.Run(string(naming)+"/nzb", func(t *testing.T) {
			t.Parallel()
			dl := t.TempDir()
			build := func(id string) *storage.Entry {
				return newNZBQueueEntry(&ImportRequest{
					ID: id, Name: "Show.S01E01", DownloadFolder: dl, Arr: arr.Arr{Name: "sonarr"},
				}, "/staged/"+id+".nzb")
			}
			a, b := build("uuid-a"), build("uuid-b")
			if a.DownloadPath(naming) == b.DownloadPath(naming) ||
				nestedIn(a.DownloadPath(naming), b.DownloadPath(naming)) ||
				nestedIn(b.DownloadPath(naming), a.DownloadPath(naming)) {
				t.Fatalf("same-named NZBs share a folder: %q and %q", a.DownloadPath(naming), b.DownloadPath(naming))
			}
			if want := filepath.Join(dl, "sonarr", "uuid-a"); a.SavePath != want || !a.OwnsSavePath() {
				t.Fatalf("SavePath = %q (owned %v), want %q owned", a.SavePath, a.OwnsSavePath(), want)
			}
		})
		t.Run(string(naming)+"/torrent", func(t *testing.T) {
			t.Parallel()
			dl := t.TempDir()
			build := func(hash string) *storage.Entry {
				return newTorrentQueueEntry(&ImportRequest{
					Magnet:         &utils.Magnet{InfoHash: hash, Name: "Show.S01E01"},
					DownloadFolder: dl, Arr: arr.Arr{Name: "sonarr"},
				}, storage.EntryStatusQueued, naming)
			}
			a, b := build("aaaa"), build("bbbb")
			if a.DownloadPath(naming) == b.DownloadPath(naming) ||
				nestedIn(a.DownloadPath(naming), b.DownloadPath(naming)) ||
				nestedIn(b.DownloadPath(naming), a.DownloadPath(naming)) {
				t.Fatalf(
					"same-named torrents share a folder: %q and %q",
					a.DownloadPath(naming),
					b.DownloadPath(naming),
				)
			}
			if want := filepath.Join(dl, "sonarr", "aaaa"); a.SavePath != want || !a.OwnsSavePath() {
				t.Fatalf("SavePath = %q (owned %v), want %q owned", a.SavePath, a.OwnsSavePath(), want)
			}
			if a.ContentPath != a.DownloadPath(naming) {
				t.Fatalf("ContentPath = %q, want %q", a.ContentPath, a.DownloadPath(naming))
			}
		})
	}
}

func TestRequestFromEntryUsesTheConfiguredRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop()})
	m.store = testConfigStore(t, func(cfg *config.Config) { cfg.DownloadFolder = root })
	e := testEntry("a")
	e.SavePath = filepath.Join(t.TempDir(), "elsewhere", "a")
	if got := m.requestFromEntry(e).DownloadFolder; got != root {
		t.Fatalf("DownloadFolder = %q, want the configured %q", got, root)
	}
}
