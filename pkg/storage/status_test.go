package storage_test

import (
	"testing"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestStatusOfPlacement(t *testing.T) {
	t.Parallel()
	cases := map[debridTypes.TorrentStatus]storage.EntryStatus{
		debridTypes.TorrentStatusRemoteQueued: storage.EntryStatusDownloading,
		debridTypes.TorrentStatusDownloading:  storage.EntryStatusDownloading,
		debridTypes.TorrentStatusDownloaded:   storage.EntryStatusDownloaded,
		debridTypes.TorrentStatusError:        storage.EntryStatusError,
		"magnet_conversion":                   storage.EntryStatusError,
	}
	for in, want := range cases {
		if got := storage.StatusOfPlacement(in); got != want {
			t.Errorf("StatusOfPlacement(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQueuedStatusesLoad(t *testing.T) {
	t.Parallel()
	pb := &storage.EntryProto{
		InfoHash: "h",
		Status:   "queued",
		Providers: map[string]*storage.ProviderEntryProto{
			"pm": {Provider: "pm", Id: "1", Status: "queued"},
		},
	}
	e := storage.ProtoToEntry(pb)
	if e.Status != storage.EntryStatusQueued {
		t.Fatalf("entry status %q, want the local queue", e.Status)
	}
	if got := e.Providers["pm"].Status; got != debridTypes.TorrentStatusRemoteQueued {
		t.Fatalf("placement status %q, want the provider's queue", got)
	}
	if !e.Providers["pm"].HoldsSlot() {
		t.Fatal("a provider-queued placement must hold its slot")
	}
	back := storage.EntryToProto(e)
	if back.GetStatus() != "queued" || back.GetProviders()["pm"].GetStatus() != "queued" {
		t.Fatalf("re-encoded statuses %q and %q, want queued for both", back.GetStatus(),
			back.GetProviders()["pm"].GetStatus())
	}
}
