package arr

import (
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// ledgerRig is a Sonarr and the ledger file the services in front of it share,
// as one install's restarts do.
type ledgerRig struct {
	fake  *researchArr
	url   string
	path  string
	clock *fakeClock
}

func newLedgerRig(t *testing.T, fake *researchArr) *ledgerRig {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return &ledgerRig{
		fake: fake, url: server.URL, path: filepath.Join(t.TempDir(), "db", "queue_cleanup_searches.db"),
		clock: &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)},
	}
}

// start is a process start: a new service that loads the ledger.
func (rig *ledgerRig) start(t *testing.T) *Service {
	t.Helper()
	cfg := config.NewStore(&config.Config{QueueCleanup: config.QueueCleanup{Rules: config.DefaultQueueCleanupRules()}})
	service := New(cfg, nil, zerolog.Nop())
	service.arrs["tv"] = Arr{Name: "tv", Host: rig.url, Token: "secret", Type: Sonarr}
	service.now = rig.clock.Now
	if err := service.OpenSearchLedger(rig.path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

// crash ends the process without letting the service do anything more.
func (rig *ledgerRig) crash(t *testing.T, service *Service) {
	t.Helper()
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func ledgerStates(t *testing.T, service *Service) map[int]ledgerState {
	t.Helper()
	rows, err := service.ledger.load()
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[int]ledgerState, len(rows))
	for _, row := range rows {
		states[row.Item.ID] = row.State
	}
	return states
}

// The search is owed before the DELETE that makes the arr forget the download:
// a row is on disk when the DELETE arrives, and armed once it succeeded.
func TestSearchIsOwedBeforeTheBlocklistDeleteIsSent(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	rig := newLedgerRig(t, fake)
	service := rig.start(t)
	var duringDelete map[int]ledgerState
	fake.onSkipDelete = func() {
		rows, err := service.ledger.load()
		if err != nil {
			t.Error(err)
		}
		duringDelete = map[int]ledgerState{}
		for _, row := range rows {
			duringDelete[row.Item.ID] = row.State
		}
	}
	fake.searchFails = true

	runCleanup(t, service, "tv", 1)
	if !slices.Equal(mapKeys(duringDelete), []int{1}) || duringDelete[1] != ledgerPlanned {
		t.Fatalf("ledger during the DELETE = %v, want record 1 planned", duringDelete)
	}
	if states := ledgerStates(t, service); states[1] != ledgerArmed {
		t.Fatalf("ledger after the DELETE = %v, want record 1 armed", states)
	}
}

func mapKeys(states map[int]ledgerState) []int {
	keys := make([]int, 0, len(states))
	for id := range states {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	return keys
}

// A DELETE that landed and whose answer was lost leaves the download gone and
// no search sent. After a restart the planned row finds its record vanished
// and the search goes out.
func TestSearchOwedByAnUnansweredDeleteIsSentAfterARestart(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	fake.deleteHangsUp = true
	rig := newLedgerRig(t, fake)
	service := rig.start(t)
	runCleanup(t, service, "tv", 1)
	fake.check(t, researchWant{skipped: []int{1}})
	if states := ledgerStates(t, service); states[1] != ledgerPlanned {
		t.Fatalf("ledger = %v, want record 1 planned", states)
	}
	rig.crash(t, service)

	restarted := rig.start(t)
	runCleanup(t, restarted, "tv", 1)
	fake.check(t, researchWant{skipped: []int{1}, searches: [][]int{{11}}})
	if states := ledgerStates(t, restarted); len(states) != 0 {
		t.Fatalf("ledger after the search = %v, want empty", states)
	}
}

// A search owed for more than the hour a live process waits is still tried
// once after a restart.
func TestOwedSearchOlderThanTheGiveUpIsTriedOnceAfterARestart(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	fake.searchFails = true
	rig := newLedgerRig(t, fake)
	service := rig.start(t)
	runCleanup(t, service, "tv", 1)
	rig.crash(t, service)

	rig.clock.advance(3 * time.Hour)
	fake.update(func(a *researchArr) { a.searchFails = false })
	restarted := rig.start(t)
	runCleanup(t, restarted, "tv", 2)
	fake.check(t, researchWant{skipped: []int{1}, searches: [][]int{{11}}})
}

// A DELETE the arr refused removed nothing and owes nothing: the row is
// abandoned, and a restart searches for nothing.
func TestRefusedDeleteAbandonsItsRow(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	fake.deleteRefused = true
	rig := newLedgerRig(t, fake)
	service := rig.start(t)
	runCleanup(t, service, "tv", 1)
	if states := ledgerStates(t, service); states[1] != ledgerAbandoned {
		t.Fatalf("ledger = %v, want record 1 abandoned", states)
	}
	rig.crash(t, service)

	fake.update(func(a *researchArr) { a.records = nil })
	restarted := rig.start(t)
	runCleanup(t, restarted, "tv", 2)
	fake.check(t, researchWant{})
	fake.update(func(a *researchArr) {
		if a.commands != 0 {
			t.Errorf("search commands sent = %d, want 0", a.commands)
		}
	})
	if states := ledgerStates(t, restarted); len(states) != 0 {
		t.Fatalf("ledger after the restart = %v, want the abandoned row dropped", states)
	}
}

// A crash between the planned row and the DELETE leaves a record still
// queued; the cleanup rules judge it again, and the row is not searched.
func TestPlannedRowOfARecordStillQueuedIsDroppedAfterARestart(t *testing.T) {
	t.Parallel()
	record := missingRecord(1, 11, "a", false)
	record.TrackedDownloadState = "importing"
	fake := newResearchArr(record)
	rig := newLedgerRig(t, fake)
	service := rig.start(t)
	planned := ledgerRow{Arr: "tv", State: ledgerPlanned, Item: record, Since: rig.clock.Now()}
	if err := service.ledger.put(planned); err != nil {
		t.Fatal(err)
	}
	rig.crash(t, service)

	restarted := rig.start(t)
	runCleanup(t, restarted, "tv", 2)
	fake.check(t, researchWant{})
	if states := ledgerStates(t, restarted); len(states) != 0 {
		t.Fatalf("ledger = %v, want empty", states)
	}
}

// With no row on disk the DELETE would leave a search nothing remembers, so it
// is not sent.
func TestBlocklistDeleteWaitsForTheLedgerToTakeTheRow(t *testing.T) {
	t.Parallel()
	fake := newResearchArr(missingRecord(1, 11, "a", false))
	rig := newLedgerRig(t, fake)
	service := rig.start(t)
	if err := service.ledger.store.Close(); err != nil {
		t.Fatal(err)
	}

	runCleanup(t, service, "tv", 1)
	fake.check(t, researchWant{})
}
