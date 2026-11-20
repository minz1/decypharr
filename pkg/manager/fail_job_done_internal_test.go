package manager

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

func failedWith(arrFail grab.ArrFail) grab.State {
	return grab.State{
		Phase:   grab.PhaseFailed,
		Failure: grab.Failure{Reason: "x", At: time.Now(), Count: 1},
		ArrFail: arrFail,
	}
}

func failJobFor(key, arrName string) reacquire.Job {
	return reacquire.Job{
		Strategy: reacquire.StrategyDownloadFailed, ArrName: arrName, EntryID: key, CreatedAt: time.Now(),
	}
}

func TestFailJobDoneRemovesFailedRecord(t *testing.T) {
	t.Parallel()
	for name, arrFail := range map[string]grab.ArrFail{
		"none":    {},
		"pending": {State: grab.ArrFailPending, Arr: "sonarr"},
		"queued":  {State: grab.ArrFailQueued, Arr: "sonarr", JobID: "job"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := deadlineHarness(t, store, noPoll)
				seed(t, store, testEntry("a"), failedWith(arrFail))
				if err := h.manager().failJobDone(t.Context(), failJobFor("a", "sonarr")); err != nil {
					t.Fatal(err)
				}
				run := h.next(grab.ChildCleanup)
				if !run.call.cleanup.DeleteFiles {
					t.Fatalf("cleanup %+v, want files deleted", run.call.cleanup)
				}
				h.mustBe("a", grab.PhaseDeleting)
			})
		})
	}
}

func TestFailJobDoneKeepsReaddedGrab(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		seed(t, store, testEntry("a"), grab.State{Phase: grab.PhaseProvider})
		var logs bytes.Buffer
		m := h.manager()
		m.logger = zerolog.New(&logs)
		if err := m.failJobDone(t.Context(), failJobFor("a", "sonarr")); err != nil {
			t.Fatal(err)
		}
		h.mustBe("a", grab.PhaseProvider)
		line := logs.String()
		if !strings.Contains(line, `"level":"info"`) || !strings.Contains(line, "refused in provider") {
			t.Fatalf("log = %s, want an INFO line with the reducer's reason", line)
		}
	})
}

func TestFailJobDoneComplete(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		completedAfterJob bool
		want              grab.Phase
	}{
		"completed before the job was queued": {false, grab.PhaseDeleting},
		"completed after the job was queued":  {true, grab.PhaseComplete},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := deadlineHarness(t, store, noPoll)
				job := failJobFor("a", "sonarr")
				since := job.CreatedAt.Add(-time.Minute)
				if tc.completedAfterJob {
					since = job.CreatedAt.Add(time.Minute)
				}
				seed(t, store, testEntry("a"), grab.State{Phase: grab.PhaseComplete, PhaseSince: since})
				if err := h.manager().failJobDone(t.Context(), job); err != nil {
					t.Fatal(err)
				}
				h.mustBe("a", tc.want)
			})
		})
	}
}

func TestFailJobDoneOtherArr(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		seed(t, store, testEntry("a"), failedWith(grab.ArrFail{State: grab.ArrFailQueued, Arr: "sonarr", JobID: "job"}))
		if err := h.manager().failJobDone(t.Context(), failJobFor("a", "radarr")); err != nil {
			t.Fatal(err)
		}
		h.mustBe("a", grab.PhaseFailed)
	})
}

func TestFailJobDoneUppercaseEntryID(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		seed(t, store, testEntry("abc"), failedWith(grab.ArrFail{}))
		if err := h.manager().failJobDone(t.Context(), failJobFor("ABC", "sonarr")); err != nil {
			t.Fatal(err)
		}
		h.mustBe("abc", grab.PhaseDeleting)
	})
}

func TestArrFailDoneSavesDurably(t *testing.T) {
	t.Parallel()
	store, queue := newCountingStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		seed(t, store, testEntry("a"), failedWith(grab.ArrFail{}))
		before := queue.syncs.Load()
		if err := h.manager().failJobDone(t.Context(), failJobFor("a", "sonarr")); err != nil {
			t.Fatal(err)
		}
		if got := queue.syncs.Load() - before; got != 1 {
			t.Fatalf("queue syncs = %d, want 1 before the reply", got)
		}
	})
}

type armedHandler struct {
	invalidator reacquire.Invalidator
	mutations   atomic.Int64
	sent        atomic.Bool
}

func (a *armedHandler) Reacquire(ctx context.Context, job reacquire.Job, _ reacquire.JobProgress) error {
	if a.sent.CompareAndSwap(false, true) {
		a.mutations.Add(1)
	}
	return a.invalidator.InvalidateReacquire(ctx, job)
}

func TestFailJobDoneSaveError(t *testing.T) {
	t.Parallel()
	store, queue := newCountingStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		seed(t, store, testEntry("a"), failedWith(grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"}))
		handler := &armedHandler{invalidator: h.manager()}
		g := task.NewRoot(t.Context(), "test", zerolog.Nop())
		service, err := reacquire.NewService(g, reacquire.ServiceOptions{
			Directory: t.TempDir(),
			Handler:   handler,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = service.Close() })

		queue.failSyncs.Store(1)
		if startErr := service.Start(t.Context()); startErr != nil {
			t.Fatal(startErr)
		}
		job, handed, err := service.FailDownload("sonarr", "DL", "a", time.Time{})
		if err != nil || handed != reacquire.HandedCreated {
			t.Fatalf("FailDownload = %v, %v", handed, err)
		}
		synctest.Wait()

		got, _ := service.Job(job.ID)
		if got.Status != reacquire.StatusQueued || !strings.Contains(got.LastError, "remove failed grab") {
			t.Fatalf("job after the failed save = %s %q, want queued with the save error", got.Status, got.LastError)
		}
		h.mustBe("a", grab.PhaseFailed)
		failed, _ := h.diskGrab("a")
		h.mustAsk("a", grab.Event{
			Kind: grab.EvArrFailQueued, Occurrence: failed.State.Failure.At, JobID: job.ID,
		})
		h.mustBe("a", grab.PhaseFailed)

		<-time.After(time.Minute)
		synctest.Wait()

		got, _ = service.Job(job.ID)
		if got.Status == reacquire.StatusFailed {
			t.Fatalf("job failed: %q", got.LastError)
		}
		h.mustBe("a", grab.PhaseDeleting)
		if handler.mutations.Load() != 1 {
			t.Fatalf("Arr mutations = %d, want 1", handler.mutations.Load())
		}
		if disk, ok := h.diskGrab("a"); !ok || disk.State.Phase != grab.PhaseDeleting {
			t.Fatalf("disk grab = %+v %t, want Deleting on disk", disk.State.Phase, ok)
		}
		if queue.syncs.Load() < 2 {
			t.Fatalf("queue syncs = %d, want the failed one and the rerun's", queue.syncs.Load())
		}
	})
}
