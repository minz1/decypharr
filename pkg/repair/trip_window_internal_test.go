package repair

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func tripLine(t *testing.T, logs *bytes.Buffer) (int, []string) {
	t.Helper()
	var trip struct {
		Confirmed int      `json:"confirmed"`
		Window    []string `json:"window"`
	}
	found := false
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "breaker tripped") {
			if err := json.Unmarshal([]byte(line), &trip); err != nil {
				t.Fatalf("trip line %q: %v", line, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no trip line in\n%s", logs.String())
	}
	return trip.Confirmed, trip.Window
}

func rfc3339At(when time.Time) string { return when.UTC().Format(time.RFC3339) }

func TestTripLogListsEveryWindowSightingWithItsSourceAndRun(t *testing.T) {
	t.Parallel()
	service, clock := newAdmitService(t)
	var logs bytes.Buffer
	service.logger = zerolog.New(&logs)
	start := clock.Now()
	trippedAt := start.Add(time.Minute)

	service.reacquirer = &journalReacquirer{jobs: []reacquire.Job{{
		ID: "job-1", EntryID: "seeded", Cause: reacquire.CauseRepair, Strategy: reacquire.StrategyCommandSearch,
		CreatedAt: start.Add(-10 * time.Minute),
	}}}
	service.seedBreakerFromJournal()
	bucket := Bucket{Provider: service.ProviderFor("seeded"), Class: ClassOther}
	if got := service.bucketOf(storage.BrokenFile{}); got != bucket {
		t.Fatalf("a bare broken file lands in %s, the seeded job in %s; the fixture needs one bucket", got, bucket)
	}
	edge := trippedAt.Add(-service.breaker.settings().Window)
	service.breaker.seed(edge, bucket, "edge", "job-0")
	service.breaker.seed(edge.Add(-time.Second), bucket, "aged", "job-old")
	service.breaker.markHeld(bucket, "aged", true)

	recheck := newProbeBatch(false, nil)
	recheck.entries["Show S01"] = &probedEntry{pending: []storage.BrokenFile{{}}}
	service.judgeBuckets(t.Context(), "recheck-1", recheck, []string{"Show S01"})
	sweep := newProbeBatch(false, nil)
	sweep.sweep = true
	sweep.entries["Film"] = &probedEntry{pending: []storage.BrokenFile{{}}}
	service.judgeBuckets(t.Context(), "sweep-1", sweep, []string{"Film"})

	clock.Advance(time.Minute)
	for i := range 6 {
		err := service.Admit(t.Context(), HealRequest{
			Provider: bucket.Provider, Class: bucket.Class, Subject: fmt.Sprintf("admit-%d", i),
		})
		if (err != nil) != (i == 5) {
			t.Fatalf("admit %d = %v; want the tenth subject in the window to trip and no earlier one", i, err)
		}
	}

	want := []string{
		rfc3339At(edge) + " source=seed run_id=job-0 subject=edge",
		rfc3339At(start.Add(-10*time.Minute)) + " source=seed run_id=job-1 subject=seeded",
		rfc3339At(start) + " source=sweep run_id=sweep-1 subject=Film",
		rfc3339At(start) + " source=run run_id=recheck-1 subject=Show S01",
	}
	for i := range 6 {
		want = append(want, fmt.Sprintf("%s source=admit run_id= subject=admit-%d", rfc3339At(trippedAt), i))
	}
	confirmed, window := tripLine(t, &logs)
	if confirmed != len(want) {
		t.Fatalf("trip confirmed = %d, want %d, the subjects the window lists", confirmed, len(want))
	}
	if !slices.Equal(window, want) {
		t.Fatalf("trip window =\n%s\nwant\n%s", strings.Join(window, "\n"), strings.Join(want, "\n"))
	}
}

func TestABatchTripOnTheRollingWindowLogsTheWindowCount(t *testing.T) {
	t.Parallel()
	service, _ := newAdmitService(t)
	var logs bytes.Buffer
	service.logger = zerolog.New(&logs)
	bucket := service.bucketOf(storage.BrokenFile{})
	for i := range 9 {
		if err := service.Admit(t.Context(), HealRequest{
			Provider: bucket.Provider, Class: bucket.Class, Subject: fmt.Sprintf("admit-%d", i),
		}); err != nil {
			t.Fatalf("admit %d = %v", i, err)
		}
	}
	recheck := newProbeBatch(false, nil)
	recheck.entries["Show S01"] = &probedEntry{pending: []storage.BrokenFile{{}}}
	plan := service.judgeBuckets(t.Context(), "recheck-1", recheck, []string{"Show S01"})
	if !plan[bucket].newlyTripped {
		t.Fatalf("plan = %+v; want the tenth subject in the window to trip the bucket", plan[bucket])
	}
	if confirmed, window := tripLine(t, &logs); confirmed != 10 || len(window) != 10 {
		t.Fatalf("trip confirmed = %d with %d window lines, want 10 and 10", confirmed, len(window))
	}
}

func TestASightingThatAgedOutTakesTheNewSource(t *testing.T) {
	t.Parallel()
	b, clock, _ := testBreaker(new(canaryOutcome))
	window := func() []string {
		b.mu.Lock()
		defer b.mu.Unlock()
		return windowLinesLocked(b.bucketLocked(testBucket()), clock.Now().Add(-b.settings().Window))
	}
	first := clock.Now().UTC().Format(time.RFC3339)
	b.noteConfirmed(testBucket(), "entry", sighting{source: sourceAdmit})
	b.noteConfirmed(testBucket(), "old", sighting{source: sourceAdmit})
	clock.Advance(30 * time.Minute)
	b.noteConfirmed(testBucket(), "entry", sighting{source: sourceRun, runID: "recheck-2"})
	want := []string{first + " source=admit run_id= subject=entry", first + " source=admit run_id= subject=old"}
	if got := window(); !slices.Equal(got, want) {
		t.Fatalf("a re-sighting inside the window keeps the first: window = %q, want %q", got, want)
	}
	clock.Advance(2 * time.Hour)
	b.noteConfirmed(testBucket(), "entry", sighting{source: sourceSweep, runID: "sweep-2"})
	want = []string{clock.Now().UTC().Format(time.RFC3339) + " source=sweep run_id=sweep-2 subject=entry"}
	if got := window(); !slices.Equal(got, want) {
		t.Fatalf("window = %q, want %q", got, want)
	}
}
