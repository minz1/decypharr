package manager

import (
	"testing"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/sirrobot01/decypharr/internal/state/statetest"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestMissMachine(t *testing.T) {
	t.Parallel()
	statetest.Table(t, missSuccessors, allMissPhases(), []statetest.Edge[missPhase]{
		{From: missNone, To: missOnce},
		{From: missOnce, To: missNone},
		{From: missOnce, To: missConfirming},
		{From: missConfirming, To: missNone},
		{From: missConfirming, To: missConfirming},
	})

	once := missState{phase: missOnce}
	confirming := missState{phase: missConfirming, recoveryFailures: 2}
	rows := []struct {
		name string
		from missState
		ev   missEvent
		want missState
		ok   bool
	}{
		{"listed from once", once, missListed, missState{}, true},
		{"listed from confirming", confirming, missListed, missState{}, true},
		{"listed from none", missState{}, missListed, missState{}, true},
		{"absent from none", missState{}, missAbsent, once, true},
		{"absent from once", once, missAbsent, missState{phase: missConfirming}, true},
		{"absent from confirming", confirming, missAbsent, confirming, true},
		{"truncated keeps once", once, missTruncated, once, true},
		{"truncated keeps confirming", confirming, missTruncated, confirming, true},
		{"found on lookup", confirming, missFoundOnLookup, missState{}, true},
		{"found on lookup before confirming", once, missFoundOnLookup, once, false},
		{"dropped", confirming, missDropped, missState{}, true},
		{"dropped before confirming", once, missDropped, once, false},
		{
			"recovery failed",
			confirming,
			missRecoveryFailed,
			missState{phase: missConfirming, recoveryFailures: 3},
			true,
		},
		{"recovery failed before confirming", missState{}, missRecoveryFailed, missState{}, false},
	}
	for _, r := range rows {
		got, ok := stepMiss(r.from, r.ev)
		if got != r.want || ok != r.ok {
			t.Errorf("%s: got %+v, %v; want %+v, %v", r.name, got, ok, r.want, r.ok)
		}
	}
}

func missesOf(m *Manager) map[string]missState {
	misses, _ := m.listingMisses.Load("rd")
	return misses
}

func TestTruncatedListingKeepsMisses(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, &fakeArrRecovery{})
	addPlaced(t, m, 1)
	client := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}

	syncTimes(t, m, client, 1)
	client.limit = 1
	syncTimes(t, m, client, 1)

	if got := missesOf(m)["hash-00"]; got.phase != missOnce {
		t.Fatalf("miss after a truncated listing = %+v, want once", got)
	}
	if got := client.lookups(); got != 0 {
		t.Fatalf("lookups=%d, want 0", got)
	}
}

func TestRestartNeedsTwoFreshMisses(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, &fakeArrRecovery{})
	addPlaced(t, m, 1)
	client := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}

	syncTimes(t, m, client, 1)
	m.listingMisses = xsync.NewMap[string, map[string]missState]()
	syncTimes(t, m, client, 1)
	if got := client.lookups(); got != 0 {
		t.Fatalf("lookups after the first fresh miss = %d, want 0", got)
	}
	syncTimes(t, m, client, 1)
	if got := client.lookups(); got != 1 {
		t.Fatalf("lookups after the second fresh miss = %d, want 1", got)
	}
}

func TestRelistingClearsMisses(t *testing.T) {
	t.Parallel()
	for _, prev := range []missState{{phase: missOnce}, {phase: missConfirming, recoveryFailures: 2}} {
		m, _ := newDroppedTestManager(t, &fakeArrRecovery{})
		addPlaced(t, m, 1)
		m.listingMisses.Store("rd", map[string]missState{"hash-00": prev})
		listed := &fakeListing{
			torrents: []*types.Torrent{otherListed(), {ID: "id-00", InfoHash: "hash-00"}},
			lookup:   goneLookup,
		}

		syncTimes(t, m, listed, 1)
		if got := missesOf(m); len(got) != 0 {
			t.Fatalf("from %+v: misses after relisting = %v, want none", prev, got)
		}
		absent := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}
		syncTimes(t, m, absent, 1)
		if got := missesOf(m)["hash-00"]; got != (missState{phase: missOnce}) {
			t.Fatalf("from %+v: next miss = %+v, want once with no failures", prev, got)
		}
	}
}

func TestSecondMissConfirms(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, &fakeArrRecovery{})
	addPlaced(t, m, 1)
	client := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}

	syncTimes(t, m, client, 1)
	if got := client.lookups(); got != 0 {
		t.Fatalf("lookups after one miss = %d, want 0", got)
	}
	syncTimes(t, m, client, 1)
	if got := client.lookups(); got != 1 {
		t.Fatalf("lookups after two misses = %d, want 1", got)
	}
}

func TestListedUnderAnotherHashClearsTheMiss(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, &fakeArrRecovery{})
	addPlaced(t, m, 1)
	m.listingMisses.Store("rd", map[string]missState{"hash-00": {phase: missOnce}})
	client := &fakeListing{
		torrents: []*types.Torrent{otherListed(), {ID: "id-00", InfoHash: "synthetic-hash"}},
		lookup:   goneLookup,
	}

	syncTimes(t, m, client, 1)

	if got := missesOf(m); len(got) != 0 {
		t.Fatalf("misses = %v, want none when the ID is listed under another hash", got)
	}
	if got := client.lookups(); got != 0 {
		t.Fatalf("lookups=%d, want 0", got)
	}
}

func TestCompleteListingForgetsUnvisitedHashes(t *testing.T) {
	t.Parallel()
	m, _ := newDroppedTestManager(t, &fakeArrRecovery{})
	m.listingMisses.Store("rd", map[string]missState{"gone": {phase: missConfirming, recoveryFailures: 3}})
	client := &fakeListing{torrents: []*types.Torrent{otherListed()}, lookup: goneLookup}

	syncTimes(t, m, client, 1)

	if _, ok := missesOf(m)["gone"]; ok {
		t.Fatalf("misses = %v, want the hash with no entry forgotten", missesOf(m))
	}
}
