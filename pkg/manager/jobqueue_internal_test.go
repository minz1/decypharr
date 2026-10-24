package manager

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// holdingQueue runs "running" until release is called and throttles "rethrottled" once during its first run.
func holdingQueue(t *testing.T) (*JobQueue, func()) {
	t.Helper()
	released := make(chan struct{})
	var q *JobQueue
	q = NewJobQueue(t.Context(), 1, func(_ context.Context, job *Job) {
		switch job.ID {
		case "running":
			<-released
		case "rethrottled":
			if job.ThrottledRetries == 0 {
				job.ThrottledRetries++
				q.WaitForSlot(job, []string{"torbox"}, time.Hour)
			}
		}
	}, zerolog.Nop())
	t.Cleanup(q.Close)
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	return q, release
}

// The stalled sweep removes a queued entry whose job the queue no longer holds; FindJob missed running jobs.
func TestJobQueueHoldsPendingAndRunningJobs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q, release := holdingQueue(t)
		_ = q.Submit(&Job{ID: "running"})
		_ = q.Submit(&Job{ID: "pending"})
		synctest.Wait()
		if !q.Holds("running") || !q.Holds("pending") {
			t.Fatalf("Holds(running) = %v, Holds(pending) = %v, want both true", q.Holds("running"), q.Holds("pending"))
		}
		if !q.DeleteJob("pending") || q.Holds("pending") {
			t.Fatal("a deleted pending job is still held")
		}
		release()
		synctest.Wait()
		if q.Holds("running") {
			t.Fatal("a job is held after its run returned")
		}
	})
}

func TestJobQueueHoldsAJobWaitingForASlot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q, _ := holdingQueue(t)
		q.WaitForSlot(&Job{ID: "waiting"}, []string{"realdebrid"}, time.Hour)
		if !q.Holds("waiting") {
			t.Fatal("a job waiting for a slot is not held")
		}
		q.SlotFreed("realdebrid")
		synctest.Wait()
		if q.Holds("waiting") {
			t.Fatal("a woken job is held after its run returned")
		}
	})
}

func TestJobQueueHoldsAJobThrottledDuringItsRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q, _ := holdingQueue(t)
		_ = q.Submit(&Job{ID: "rethrottled"})
		synctest.Wait()
		if !q.Holds("rethrottled") {
			t.Fatal("a job throttled again during its run is not held")
		}
		q.SlotFreed("torbox")
		synctest.Wait()
		if q.Holds("rethrottled") {
			t.Fatal("a re-throttled job is held after its second run returned")
		}
	})
}

// Until the persisted queue is resubmitted, a queued entry may still be waiting for its job.
func TestQueuedEntryAwaitsAJobUntilTheQueueIsRestored(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	m := &Manager{ctx: ctx, jobQueue: NewJobQueue(ctx, 1, func(context.Context, *Job) {}, zerolog.Nop())}
	t.Cleanup(m.jobQueue.Close)
	if !m.awaitsJob("hash") {
		t.Fatal("awaitsJob = false before the restore")
	}
	m.queueRestored.Store(true)
	if m.awaitsJob("hash") {
		t.Fatal("awaitsJob = true after the restore for a hash with no job")
	}
	m.jobQueue.WaitForSlot(&Job{ID: "waiting"}, []string{"torbox"}, time.Hour)
	if !m.awaitsJob("waiting") {
		t.Fatal("awaitsJob = false for a job waiting for a slot")
	}
	// Shutdown cancels every waiter before the sweep stops; restore resubmits them on the next start.
	cancel()
	if !m.awaitsJob("hash") {
		t.Fatal("awaitsJob = false after shutdown started")
	}
}

// A restore that stopped early resubmitted only some queued entries; the rest must not look abandoned.
func TestOnlyAFullRestoreMarksTheQueueRestored(t *testing.T) {
	t.Parallel()
	for name, closed := range map[string]bool{"full pass": false, "scan failed": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if closed {
				_ = store.Close()
			} else {
				t.Cleanup(func() { _ = store.Close() })
			}
			m := &Manager{queue: newQueue(store, "", nil, zerolog.Nop()), logger: zerolog.Nop(), ctx: t.Context()}
			m.restoreActiveDownloadJobs(m.restoreSnapshot())
			if m.queueRestored.Load() == closed {
				t.Fatalf("queueRestored = %v after a %s", m.queueRestored.Load(), name)
			}
		})
	}
}
