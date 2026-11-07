package arr

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestQueueGuardTransitionTableAcceptsExactlyTheLegalEdges(t *testing.T) {
	t.Parallel()
	states := []guardState{guardClosed, guardTripped}
	legal := map[[2]guardState]bool{
		{guardClosed, guardTripped}: true,
		{guardTripped, guardClosed}: true,
	}
	at := time.Unix(1000, 0)
	for _, from := range states {
		for _, to := range states {
			bucket := &guardBucket{state: from}
			err := bucket.transition(to, at)
			switch want := legal[[2]guardState{from, to}]; {
			case want && (err != nil || bucket.state != to):
				t.Errorf("%s -> %s: err %v state %s, want accepted", from, to, err, bucket.state)
			case !want && err == nil:
				t.Errorf("%s -> %s accepted, want refused", from, to)
			case !want && bucket.state != from:
				t.Errorf("a refused %s -> %s moved the bucket to %s", from, to, bucket.state)
			}
		}
	}
	for _, from := range states {
		for _, to := range from.successors() {
			if !legal[[2]guardState{from, to}] {
				t.Errorf("successors() lists %s -> %s, which the table does not allow", from, to)
			}
		}
	}
}

// guardHooks is a stand-in for the manager: it names providers, answers the canary and counts what it is asked.
type guardHooks struct {
	mu        sync.Mutex
	providers map[string]string
	verdict   CanaryVerdict
	canaries  []string
	notices   []string
}

func (g *guardHooks) hooks() QueueGuardHooks {
	return QueueGuardHooks{
		Provider: func(downloadID string) string {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.providers[downloadID]
		},
		Canary: func(_ context.Context, provider string) CanaryVerdict {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.canaries = append(g.canaries, provider)
			return g.verdict
		},
		Notify: func(_, message string) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.notices = append(g.notices, message)
		},
	}
}

func (g *guardHooks) setVerdict(verdict CanaryVerdict) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.verdict = verdict
}

func (g *guardHooks) canaryCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.canaries)
}

func (g *guardHooks) noticeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.notices)
}

// guardedService is a Sonarr service on a fake clock with the guard hooks installed.
func guardedService(t *testing.T, fake *researchArr) (*Service, *fakeClock, *guardHooks) {
	t.Helper()
	service := researchService(t, fake, Sonarr)
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	service.now = clock.Now
	hooks := &guardHooks{providers: map[string]string{}, verdict: CanaryPass}
	service.SetQueueGuardHooks(hooks.hooks())
	return service, clock, hooks
}

// packRows is the queue rows of one pack download: a row per episode, ids from firstID.
func packRows(downloadID string, firstID, episodes int) []QueueSchema {
	rows := make([]QueueSchema, episodes)
	for i := range rows {
		rows[i] = missingRecord(firstID+i, 1000*firstID+i, downloadID, false)
	}
	return rows
}

// failedRows is the rows of a failed download the arr could not map to episodes.
func failedRows(downloadID string, firstID, count int) []QueueSchema {
	rows := make([]QueueSchema, count)
	for i := range rows {
		rows[i] = QueueSchema{ID: firstID + i, DownloadID: downloadID, Status: "failed", Protocol: "usenet"}
	}
	return rows
}

func queueIDs(fake *researchArr) []int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	ids := make([]int, len(fake.records))
	for i, record := range fake.records {
		ids[i] = record.ID
	}
	return ids
}

func blocklisted(fake *researchArr) []int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return slices.Concat(fake.deletes["true"], fake.deletes["false"])
}

// The 2026-10-20 burst: 88 pack downloads rejected within minutes, a row per episode. They are delayed across
// windows, never dropped, and each is blocklisted exactly once.
func TestBlocklistBurstIsDelayedAcrossWindowsAndEveryItemSentOnce(t *testing.T) {
	t.Parallel()
	var records []QueueSchema
	for i := range 88 {
		records = append(records, packRows(fmt.Sprintf("pack-%02d", i), 10+i*10, 3)...)
	}
	fake := newResearchArr(records...)
	service, clock, _ := guardedService(t, fake)

	runCleanup(t, service, "tv", 1)
	if got := len(blocklisted(fake)); got != 60 {
		t.Fatalf("first window blocklisted %d rows, want 20 downloads x 3 rows", got)
	}
	// The held ones owe no search: only the episodes of the 20 removed downloads are searched, and nothing is
	// left planned or armed for a row still in the queue.
	fake.mu.Lock()
	searched := len(slices.Concat(fake.searches...))
	fake.mu.Unlock()
	service.mu.RLock()
	owed := len(service.pendingSearches["tv"])
	service.mu.RUnlock()
	if searched != 60 || owed != 0 {
		t.Fatalf(
			"searched %d episodes with %d still owed, want the 60 of the sent downloads and none owed",
			searched,
			owed,
		)
	}
	if left := len(queueIDs(fake)); left != 88*3-60 {
		t.Fatalf("queue rows left = %d, want the held ones in place", left)
	}
	runCleanup(t, service, "tv", 5) // same window: nothing more passes
	if got := len(blocklisted(fake)); got != 60 {
		t.Fatalf("a later tick of the same window blocklisted %d rows, want 60", got)
	}

	windows := 1
	for len(queueIDs(fake)) > 0 && windows < 10 {
		clock.advance(61 * time.Minute)
		runCleanup(t, service, "tv", 1)
		windows++
	}
	if windows != 5 {
		t.Fatalf("burst drained in %d windows, want 5 (88 downloads at 20 an hour)", windows)
	}
	sent := blocklisted(fake)
	slices.Sort(sent)
	if len(sent) != 88*3 || len(slices.Compact(slices.Clone(sent))) != len(sent) {
		t.Fatalf("blocklisted %d rows (%d distinct), want each of the %d exactly once", len(sent),
			len(slices.Compact(slices.Clone(sent))), 88*3)
	}
}

func TestBlocklistCountsADownloadOnceHoweverManyRows(t *testing.T) {
	t.Parallel()
	records := packRows("big-pack", 1, 100)
	for i := range 19 {
		records = append(records, packRows(fmt.Sprintf("other-%02d", i), 1000+i*10, 2)...)
	}
	fake := newResearchArr(records...)
	service, _, _ := guardedService(t, fake)

	runCleanup(t, service, "tv", 1)
	if left := len(queueIDs(fake)); left != 0 {
		t.Fatalf("%d rows left: a 100-episode pack must count as one of the 20 downloads", left)
	}
}

func TestFailedDownloadStormWithAFailingCanaryTripsAndBlocklistsNothingMore(t *testing.T) {
	t.Parallel()
	for name, verdict := range map[string]CanaryVerdict{"failing": CanaryFail, "inconclusive": CanaryInconclusive} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stormTripsAndHolds(t, verdict)
		})
	}
}

func stormTripsAndHolds(t *testing.T, verdict CanaryVerdict) {
	t.Helper()
	var records []QueueSchema
	for i := range 50 {
		records = append(records, failedRows(fmt.Sprintf("dl-%02d", i), 1+i*2, 2)...)
	}
	fake := newResearchArr(records...)
	service, clock, hooks := guardedService(t, fake)
	hooks.setVerdict(verdict)

	runCleanup(t, service, "tv", 3)
	if got := len(blocklisted(fake)); got != 40 {
		t.Fatalf("blocklisted %d rows, want the 20 free downloads only", got)
	}
	if hooks.noticeCount() != 1 {
		t.Fatalf("notifications = %d, want one for the trip", hooks.noticeCount())
	}
	if hooks.canaryCalls() != 1 {
		t.Fatalf("canary calls = %d, want the verdict reused within its cache", hooks.canaryCalls())
	}
	if status := service.QueueGuardStatus(); len(status) != 1 || status[0].State != "tripped" ||
		status[0].Rule != "failed_download" {
		t.Fatalf("guard status = %+v, want failed_download tripped", status)
	}

	// The provider recovers; once the cached verdict expires the held ones go through.
	hooks.setVerdict(CanaryPass)
	clock.advance(16 * time.Minute)
	runCleanup(t, service, "tv", 1)
	if left := len(queueIDs(fake)); left != 0 {
		t.Fatalf("%d rows still queued after a passing canary", left)
	}
	if status := service.QueueGuardStatus(); len(status) != 0 {
		t.Fatalf("guard status after release = %+v, want closed", status)
	}
}

func TestFailedDownloadsBeyondTheFreeTwentyPassWhileTheCanaryPasses(t *testing.T) {
	t.Parallel()
	var records []QueueSchema
	for i := range 50 {
		records = append(records, failedRows(fmt.Sprintf("dl-%02d", i), 1+i, 1)...)
	}
	fake := newResearchArr(records...)
	service, _, hooks := guardedService(t, fake)

	runCleanup(t, service, "tv", 1)
	if left := len(queueIDs(fake)); left != 0 {
		t.Fatalf("%d failed downloads held although the canary passes", left)
	}
	if hooks.canaryCalls() != 1 {
		t.Fatalf("canary calls = %d, want 1", hooks.canaryCalls())
	}
}

func TestBucketsAreIndependentPerProviderAndTheArrHasAHardCap(t *testing.T) {
	t.Parallel()
	var records []QueueSchema
	hooks := map[string]string{}
	for i := range 130 {
		id := fmt.Sprintf("dl-%03d", i)
		records = append(records, packRows(id, 1+i*10, 1)...)
		hooks[id] = fmt.Sprintf("provider-%d", i%13) // 10 per provider: under 20 each
	}
	fake := newResearchArr(records...)
	service, clock, guard := guardedService(t, fake)
	guard.providers = hooks

	runCleanup(t, service, "tv", 1)
	if got := len(blocklisted(fake)); got != 100 {
		t.Fatalf("blocklisted %d downloads in the first window, want the arr cap of 100", got)
	}
	clock.advance(61 * time.Minute)
	runCleanup(t, service, "tv", 1)
	if left := len(queueIDs(fake)); left != 0 {
		t.Fatalf("%d rows left after the next window", left)
	}
}

func TestOtherRulesHoldBeyondTwentyWithoutACanary(t *testing.T) {
	t.Parallel()
	var records []QueueSchema
	for i := range 25 {
		records = append(records, packRows(fmt.Sprintf("dl-%02d", i), 1+i*10, 1)...)
	}
	fake := newResearchArr(records...)
	service, _, hooks := guardedService(t, fake)

	runCleanup(t, service, "tv", 2)
	if got := len(blocklisted(fake)); got != 20 {
		t.Fatalf("blocklisted %d, want 20", got)
	}
	if hooks.canaryCalls() != 0 {
		t.Fatalf("a rule without a canary asked for %d", hooks.canaryCalls())
	}
}

func TestARefusedBlocklistDoesNotUseUpTheWindow(t *testing.T) {
	t.Parallel()
	var records []QueueSchema
	for i := range 25 {
		records = append(records, packRows(fmt.Sprintf("dl-%02d", i), 1+i*10, 1)...)
	}
	fake := newResearchArr(records...)
	fake.deleteRefused = true
	service, _, _ := guardedService(t, fake)

	runCleanup(t, service, "tv", 3)
	fake.update(func(a *researchArr) { a.deleteRefused = false })
	runCleanup(t, service, "tv", 1)
	if got := len(blocklisted(fake)); got != 20 {
		t.Fatalf("blocklisted %d after the arr stopped refusing, want 20: refused sends must not count", got)
	}
	// The refused send planned owed searches and then abandoned them; none may linger for the held rows.
	service.mu.RLock()
	defer service.mu.RUnlock()
	for id, search := range service.pendingSearches["tv"] {
		if !slices.Contains(blocklisted(fake), id) {
			t.Errorf("owed search for row %d which was never removed (%v)", id, search.state)
		}
	}
}
