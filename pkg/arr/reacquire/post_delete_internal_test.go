package reacquire

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// confirmedSearchOrGrab reports whether the job owns a search or grab the Arr
// is known to have taken.
func confirmedSearchOrGrab(job Job) bool {
	return slices.ContainsFunc(job.Mutations, func(mutation Mutation) bool {
		return mutation.State == MutationConfirmed && mutation.Kind != MutationHistoryFailed
	})
}

// requestsAfterADeleteIn runs the job once against a fake that offers an
// eligible release and returns the requests made after the file was gone.
func requestsAfterADeleteIn(t *testing.T, strategy Strategy) []string {
	t.Helper()
	fake := newFakeSonarr(t, 101)
	fake.offerRelease(true)
	rig := newFakeSonarrRig(t, fake, strategy, 101)
	if job := rig.run(t); !job.Status.waiting() {
		t.Fatalf("clean run of %s ended %s: %s", strategy, job.Status, job.LastError)
	}
	steps := fake.requestsAfterDelete()
	if len(steps) == 0 {
		t.Fatalf("clean run of %s made no request after the delete", strategy)
	}
	return steps
}

// Whatever step after the delete is refused, the job does not end failed
// unless the Arr took a search or a grab: the file is gone, so a failed job
// would leave the media missing with nothing searching for it.
func TestNoStepAfterTheDeleteFailsTheJobWithoutASearch(t *testing.T) {
	t.Parallel()
	for _, strategy := range []Strategy{StrategyCommandSearch, StrategyHistoryFailed, StrategyInteractiveBest} {
		t.Run(string(strategy), func(t *testing.T) {
			t.Parallel()
			for index, step := range requestsAfterADeleteIn(t, strategy) {
				t.Run(fmt.Sprintf("%d %s", index, step), func(t *testing.T) {
					t.Parallel()
					runWithAStepRefused(t, strategy, index)
				})
			}
		})
	}
}

// runWithAStepRefused drives a job whose index-th request after the delete is
// refused until it settles, and checks it never failed without a search.
func runWithAStepRefused(t *testing.T, strategy Strategy, index int) {
	t.Helper()
	fake := newFakeSonarr(t, 101)
	fake.offerRelease(true)
	fake.failRequestAfterDelete(index)
	rig := newFakeSonarrRig(t, fake, strategy, 101)
	for range 4 {
		job := rig.run(t)
		if job.Status == StatusFailed && !confirmedSearchOrGrab(job) {
			t.Fatalf("job failed after the delete with no search or grab taken: %s", job.LastError)
		}
		if job.Status != StatusQueued {
			return
		}
	}
}

// A refused release search leaves nothing the Arr will search for. The job
// sends one search of its own, says so, and stops for an operator.
func TestFailureAfterTheDeleteSendsOneCompensatingSearch(t *testing.T) {
	t.Parallel()
	steps := requestsAfterADeleteIn(t, StrategyInteractiveBest)
	fake := newFakeSonarr(t, 101)
	fake.offerRelease(true)
	fake.failRequestAfterDelete(slices.Index(steps, "GET /api/v3/release"))
	rig := newFakeSonarrRig(t, fake, StrategyInteractiveBest, 101)

	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
	if searches := fake.searchCount(); searches != 1 {
		t.Fatalf("searches = %d, want the one compensating search: %s", searches, stopped.LastError)
	}
	compensating, found := mutationForKey(stopped, mutationKey(MutationEpisodeSearch, "101")+compensatingKeySuffix)
	if !found || compensating.State != MutationConfirmed {
		t.Fatalf("compensating search = %+v (found %v)", compensating, found)
	}
	for _, want := range []string{"search releases", "compensating search was sent", "episodes are monitored again"} {
		if !strings.Contains(stopped.LastError, want) {
			t.Errorf("last error %q does not say %q", stopped.LastError, want)
		}
	}
	requireMonitored(t, fake, 101)
}

// With no release the Arr would accept, an interactive reacquire searches.
func TestInteractiveBestWithoutAnEligibleReleaseSearches(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.offerRelease(false)
	rig := newFakeSonarrRig(t, fake, StrategyInteractiveBest, 101)

	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	if searches, grabs := fake.searchCount(), fake.grabCount(); searches != 1 || grabs != 0 {
		t.Fatalf("searches = %d, grabs = %d, want 1 and 0", searches, grabs)
	}
}

// A job that deleted an Arr file and waited six hours for a replacement is
// searched for once more and left for an operator, not failed.
func TestExpiredWaitingJobWithADeletedFileSearchesAgain(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	if err := rig.service.SetHandler(rig.handler); err != nil {
		t.Fatal(err)
	}

	rig.advance(waitingTimeout + time.Minute)
	rig.service.maintainJobs()
	expired, _ := rig.service.Job(rig.job.ID)
	if expired.Status != StatusNeedsAttention {
		t.Fatalf("expired job = %s: %s", expired.Status, expired.LastError)
	}
	if searches := fake.searchCount(); searches != 2 {
		t.Fatalf("searches = %d, want the original and one compensating", searches)
	}
	if !strings.Contains(expired.LastError, "not imported") ||
		!strings.Contains(expired.LastError, "compensating search was sent") {
		t.Fatalf("last error = %q", expired.LastError)
	}
	rig.service.maintainJobs()
	if searches := fake.searchCount(); searches != 2 {
		t.Fatalf("a stopped job was searched again: %d searches", searches)
	}
}

// The maintenance pass that searches for an expired job runs beside the worker. The job must stay out of the
// worker's reach while it does: a dispatchable status would let the worker run the job against the same Arr, or
// stop it at the reconciliation deadline before the search is sent.
func TestExpiredJobIsNotDispatchableWhileItIsSearchedFor(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	var seen []Status
	fake.onCommand = func() {
		if job, ok := rig.service.Job(rig.job.ID); ok {
			seen = append(seen, job.Status)
		}
	}
	if err := rig.service.SetHandler(rig.handler); err != nil {
		t.Fatal(err)
	}
	rig.advance(waitingTimeout + time.Minute)
	rig.service.expireWaitingJob(t.Context(), rig.job.ID)

	// Exactly the waiting status it had: a worker that took the job first would have stopped it, leaving
	// needs_attention, which is no more dispatchable than waiting but means the search never went out held.
	if len(seen) != 1 || seen[0] != StatusWaitingForGrab {
		t.Fatalf("status when the compensating search arrived = %v, want [%s]", seen, StatusWaitingForGrab)
	}
}

// A replacement that is indexed while the expired job is searched for completes it: the media exists again, so the
// job ends ready rather than in needs_attention, and nothing is reported as an error.
func TestReplacementLandingDuringTheExpirySearchEndsTheJobReady(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	fake.onCommand = func() {
		replacement := fakeSonarrBinding(fake.instance(), 101)
		replacement.EntryID, replacement.EntryFileID, replacement.DownloadID = "entry-2", "file-2", "download-2"
		replacement.ArrFileID, replacement.LibraryPath = fakeSonarrFileID+1, "/library/show/s01e01-new.mkv"
		if err := rig.service.UpsertBinding(replacement); err != nil {
			t.Errorf("index the replacement: %v", err)
		}
	}
	if err := rig.service.SetHandler(rig.handler); err != nil {
		t.Fatal(err)
	}
	rig.advance(waitingTimeout + time.Minute)
	rig.service.expireWaitingJob(t.Context(), rig.job.ID)

	done, _ := rig.service.Job(rig.job.ID)
	if done.Status != StatusReady {
		t.Fatalf("job = %s: %s", done.Status, done.LastError)
	}
	if notices := rig.announced.all(); len(notices) != 0 {
		t.Fatalf("notices = %+v, want none", notices)
	}
	logged := rig.logs.String()
	if strings.Contains(logged, `"level":"error"`) || strings.Contains(logged, `"level":"warn"`) {
		t.Fatalf("a replacement that completed the job was logged as a problem: %s", logged)
	}
	if !strings.Contains(logged, "completed while it was searched for") {
		t.Fatalf("the completion was not logged at debug: %s", logged)
	}
}

// A job that left its waiting status between the maintenance snapshot and the expiry is not touched, whatever it
// became: held at a new status, it would lose the work it is doing. The statuses here are ones the worker leaves
// alone, so the test does not race it.
func TestExpiryLeavesAJobThatIsNoLongerWaiting(t *testing.T) {
	t.Parallel()
	for _, moved := range []Status{StatusReady, StatusNeedsAttention} {
		t.Run(string(moved), func(t *testing.T) {
			t.Parallel()
			fake := newFakeSonarr(t, 101)
			rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
			if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab {
				t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
			}
			if err := rig.service.SetHandler(rig.handler); err != nil {
				t.Fatal(err)
			}
			if _, err := rig.service.updateJob(rig.job.ID, moved, nil); err != nil {
				t.Fatal(err)
			}

			rig.service.expireWaitingJob(t.Context(), rig.job.ID)
			left, _ := rig.service.Job(rig.job.ID)
			if left.Status != moved || fake.searchCount() != 1 {
				t.Fatalf(
					"job = %s, searches = %d, want it left %s with no new search",
					left.Status,
					fake.searchCount(),
					moved,
				)
			}
		})
	}
}

// A job re-queued while the search is sent stays queued: the updates the search makes check, under the jobs lock,
// that the job is still waiting, so none puts it back to waiting or stops it. The retry time is in the future so the
// worker leaves the queued job alone.
func TestJobRequeuedDuringTheExpirySearchStaysQueued(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	fake.onCommand = func() {
		if _, err := rig.service.updateJob(rig.job.ID, StatusQueued, func(job *Job) {
			job.RetryAt = rig.service.now().Add(time.Hour)
		}); err != nil {
			t.Errorf("re-queue the job: %v", err)
		}
	}
	if err := rig.service.SetHandler(rig.handler); err != nil {
		t.Fatal(err)
	}
	rig.advance(waitingTimeout + time.Minute)
	rig.service.expireWaitingJob(t.Context(), rig.job.ID)

	requeued, _ := rig.service.Job(rig.job.ID)
	if requeued.Status != StatusQueued {
		t.Fatalf("job = %s: %s, want it left queued", requeued.Status, requeued.LastError)
	}
	if notices := rig.announced.all(); len(notices) != 0 {
		t.Fatalf("notices = %+v, want none", notices)
	}
}

func TestExpiryDoesNotFailAJobThatIsNoLongerWaiting(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)

	rig.service.expireWaitingJob(t.Context(), rig.job.ID)
	queued, _ := rig.service.Job(rig.job.ID)
	if queued.Status != StatusQueued {
		t.Fatalf("job = %s: %s, want it left queued", queued.Status, queued.LastError)
	}
}

// A handler that cannot send a search still does not fail the job.
func TestFailureAfterTheDeleteWithoutASearchingHandlerStopsTheJob(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	now := time.Now().UTC()
	seedFileDelete(t, rig, FileDelete{
		ArrFileID: fakeSonarrFileID, EpisodeIDs: []int{101}, AttemptedAt: now, ConfirmedAt: now,
		Attempts: 1, Remonitored: true,
	})
	rig.handler = reconciliationHandler(func(context.Context, Job, JobProgress) error {
		return errors.New("boom")
	})

	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention || !strings.Contains(stopped.LastError, "boom") ||
		!strings.Contains(stopped.LastError, "no compensating search") {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
}
