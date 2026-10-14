package manager

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
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
	if entry := newTorrentQueueEntry(req, debridTypes.TorrentStatusQueued, ""); !entry.SkipMultiSeason {
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
			torrentEntry := newTorrentQueueEntry(torrentReq, debridTypes.TorrentStatusDownloading, "")
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
