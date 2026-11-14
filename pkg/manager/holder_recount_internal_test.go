package manager

import (
	"slices"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestARecountCountsASameKeyLibraryRecordAsAHolder(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	h.addEntry(t, "self", shareID("dup"))

	rc, err := h.m.holderRecount("torbox", "dup", holderCall{releasing: "self"}, nil)
	if err != nil || !rc.held {
		t.Fatalf("recount = %+v, %v; want the grab's own library record to count", rc, err)
	}
	rc, err = h.m.holderRecount("torbox", "dup", holderCall{releasing: "self", library: true}, nil)
	if err != nil || rc.held {
		t.Fatalf("recount = %+v, %v; want a library record being removed not to count", rc, err)
	}
}

func TestALibraryReleaseCountsASameKeyGrabAsAHolder(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	e := &storage.Entry{
		InfoHash: "self", Name: "self", State: storage.EntryStateDownloading,
		Providers: map[string]*storage.ProviderEntry{"torbox": {Provider: "torbox", ID: "dup"}},
	}
	if err := h.m.queue.Add(e); err != nil {
		t.Fatal(err)
	}

	rc, err := h.m.holderRecount("torbox", "dup", holderCall{releasing: "self", library: true}, nil)
	if err != nil || !rc.held {
		t.Fatalf("recount = %+v, %v; want a live grab of the pruned key to keep the torrent", rc, err)
	}
	rc, err = h.m.holderRecount("torbox", "dup", holderCall{releasing: "self"}, nil)
	if err != nil || rc.held {
		t.Fatalf("recount = %+v, %v; want a grab's own release not to count its record", rc, err)
	}
}

func TestARecountDropsHoldsWithNoRecordOrAnotherTorrent(t *testing.T) {
	t.Parallel()
	h := newPruneHarness(t, config.PruneModeDelete)
	queued := func(key, id string) {
		e := &storage.Entry{
			InfoHash: key, Name: key, State: storage.EntryStateDownloading,
			Providers: map[string]*storage.ProviderEntry{"torbox": {Provider: "torbox", ID: id}},
		}
		if err := h.m.queue.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	queued("moved", "other")

	rc, err := h.m.holderRecount("torbox", "dup", holderCall{releasing: "self"}, []string{"ghost", "moved"})
	if err != nil || rc.held || !slices.Equal(rc.stale, []string{"ghost", "moved"}) {
		t.Fatalf("recount = %+v, %v; want both holds stale and nothing held", rc, err)
	}

	queued("live", "dup")
	rc, err = h.m.holderRecount("torbox", "dup", holderCall{releasing: "self"}, []string{"ghost", "live"})
	if err != nil || !rc.held || !slices.Equal(rc.stale, []string{"ghost"}) {
		t.Fatalf("recount = %+v, %v; want a live hold to count and the ghost stale", rc, err)
	}
}
