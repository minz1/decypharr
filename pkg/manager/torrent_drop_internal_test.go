package manager

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func dropEntry(providers ...string) *storage.Entry {
	e := &storage.Entry{InfoHash: "h1", Providers: map[string]*storage.ProviderEntry{}}
	for _, p := range providers {
		e.Providers[p] = &storage.ProviderEntry{Provider: p}
	}
	return e
}

func TestDropNeedsTwoConsecutiveMisses(t *testing.T) {
	t.Parallel()
	var c torrentChanges
	e := dropEntry("A")
	c.classify("A", e, nil, false)
	if len(c.delete) != 0 || len(c.update) != 0 {
		t.Fatalf("first miss acted: %+v", c)
	}
	if _, ok := c.misses["h1"]; !ok {
		t.Fatal("first miss not recorded")
	}
	if _, ok := e.Providers["A"]; !ok {
		t.Fatal("provider removed on first miss")
	}
	var c2 torrentChanges
	c2.classify("A", e, nil, true)
	if len(c2.delete) != 1 || c2.delete[0] != e {
		t.Fatalf("second miss did not delete: %+v", c2)
	}
}

func TestMultiProviderDropNeedsTwoMisses(t *testing.T) {
	t.Parallel()
	e := dropEntry("A", "B")
	var c torrentChanges
	c.classify("A", e, nil, false)
	if len(c.update) != 0 || e.Providers["A"] == nil {
		t.Fatalf("first miss acted: %+v", c)
	}
	var c2 torrentChanges
	c2.classify("A", e, nil, true)
	if len(c2.update) != 1 || len(c2.delete) != 0 {
		t.Fatalf("second miss: %+v", c2)
	}
	if _, ok := e.Providers["A"]; ok {
		t.Fatal("A not removed")
	}
}

func TestReappearingTorrentResetsMiss(t *testing.T) {
	t.Parallel()
	var c torrentChanges
	c.classify("A", dropEntry("A"), &types.Torrent{InfoHash: "h1"}, true)
	if _, ok := c.misses["h1"]; ok {
		t.Fatal("present torrent recorded as miss")
	}
}
