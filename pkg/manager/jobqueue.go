package manager

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

// JobType represents the type of processing job.
type JobType string

const (
	JobTypeTorrent JobType = "torrent"
	JobTypeNZB     JobType = "nzb"
)

// Job represents a unified processing job for both torrents and NZBs.
type Job struct {
	ID             string
	Type           JobType
	Request        *ImportRequest               // The original import request
	DebridTorrent  *debridTypes.Torrent         // Torrent placement created before the active-download gate
	NZBMeta        *storage.NZB                 // NZB metadata parsed before the active-download gate
	NZBGroups      map[string]*parser.FileGroup // NZB file groups parsed before the active-download gate
	Entry          *storage.Entry               // Entry created during processing
	ResumeExisting bool                         // Continue an already persisted provider placement
	CreatedAt      time.Time
	// ThrottledRetries counts retries after the provider's active-download cap.
	ThrottledRetries int
	// ParseRounds counts NZB parse rounds that ran out of time.
	ParseRounds int
	// wokenBy is the provider whose freed slot resubmitted the job, if one did.
	wokenBy string
}

// NewJob creates a new job.
func NewJob(jobType JobType, req *ImportRequest) *Job {
	id := ""
	if req != nil {
		id = req.ID
	}
	return &Job{
		ID:        id,
		Type:      jobType,
		Request:   req,
		CreatedAt: time.Now(),
	}
}

// JobQueue is a unified, unbounded, thread-safe job queue with a fixed worker pool.
// It replaces the separate ImportRequest queue, nzbJobQueue, and unbounded goroutine
// fan-out with a single queue that processes both torrent and NZB jobs.
type JobQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	jobs   []*Job
	closed bool
	// waiters are throttled jobs, oldest job first.
	waiters []*slotWaiter
	// held counts, per job ID, the places that will still run the job: pending, waiting for a slot or running.
	held map[string]int

	maxWorkers int
	logger     zerolog.Logger
	active     atomic.Int64

	// processFunc is called by workers to process a job
	processFunc func(ctx context.Context, job *Job)
	// group owns the workers and the slot waiters; its context ends them.
	group *task.Group
}

type slotWaiter struct {
	createdAt time.Time
	providers []string
	wake      chan struct{}
	freedBy   string
}

// jobQueueInitialCapacity presizes the pending-job slice.
const jobQueueInitialCapacity = 64

// NewJobQueue creates a job queue whose workers and slot waiters are tasks of
// g. Stopping g closes the queue: pending jobs are dropped (their records stay
// in storage and are restored on the next start) and running jobs see their
// context end. A queue made on a group that is not running refuses every job.
func NewJobQueue(
	g *task.Group,
	maxWorkers int,
	processFunc func(ctx context.Context, job *Job),
	log zerolog.Logger,
) *JobQueue {
	if maxWorkers <= 0 {
		maxWorkers = defaultFileDownloadWorkers
	}

	q := &JobQueue{
		jobs:        make([]*Job, 0, jobQueueInitialCapacity),
		held:        make(map[string]int),
		maxWorkers:  maxWorkers,
		logger:      log,
		processFunc: processFunc,
		group:       g,
	}
	q.cond = sync.NewCond(&q.mu)

	// The closer wakes the workers, which wait on the condition variable
	// rather than on the context, when the group stops.
	if err := g.Go("closer", func(ctx context.Context) error {
		<-ctx.Done()
		q.shutdown()
		return nil
	}); err != nil {
		q.shutdown()
		return q
	}
	for i := range maxWorkers {
		if err := g.Go("worker", func(context.Context) error {
			q.worker(i)
			return nil
		}); err != nil {
			q.shutdown()
			break
		}
	}

	q.logger.Info().Int("workers", maxWorkers).Msg("Job queue started")
	return q
}

// Submit adds a job to the queue (never blocks).
func (q *JobQueue) Submit(job *Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return fmt.Errorf("job queue: %w", task.ErrStopped)
	}

	q.jobs = append(q.jobs, job)
	q.held[job.ID]++
	q.cond.Signal() // Wake one waiting worker
	q.logger.Debug().
		Str("id", job.ID).
		Str("type", string(job.Type)).
		Int("queued", len(q.jobs)).
		Msg("Job submitted")
	return nil
}

// Len returns the current number of pending jobs.
func (q *JobQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// ActiveCount returns the number of jobs that workers are processing.
func (q *JobQueue) ActiveCount() int {
	return int(q.active.Load())
}

// WaitForSlot submits a throttled job again once SlotFreed names one of its providers, or after delay at the latest.
// With no providers the job is submitted again after delay.
// shortcut: a slot freed between the provider's refusal and this call wakes nobody; the job waits out delay.
func (q *JobQueue) WaitForSlot(job *Job, providers []string, delay time.Duration) {
	w := &slotWaiter{createdAt: job.CreatedAt, providers: providers, wake: make(chan struct{})}
	q.mu.Lock()
	// A job throttled again keeps its place among the jobs that were throttled after it.
	i := slices.IndexFunc(q.waiters, func(other *slotWaiter) bool { return other.createdAt.After(w.createdAt) })
	if i < 0 {
		i = len(q.waiters)
	}
	q.waiters = slices.Insert(q.waiters, i, w)
	q.held[job.ID]++
	q.mu.Unlock()
	if err := q.group.Go("wait-slot", func(ctx context.Context) error {
		defer q.release(job.ID)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-w.wake:
		case <-timer.C:
		}
		q.mu.Lock()
		q.waiters = slices.DeleteFunc(q.waiters, func(other *slotWaiter) bool { return other == w })
		job.wokenBy = w.freedBy
		q.mu.Unlock()
		if submitErr := q.Submit(job); submitErr != nil {
			q.logger.Debug().Err(submitErr).Str("job_id", job.ID).Msg("Failed to retry job")
		}
		return nil
	}); err != nil {
		// The queue is stopping: the job's record stays in storage for the next start.
		q.mu.Lock()
		q.waiters = slices.DeleteFunc(q.waiters, func(other *slotWaiter) bool { return other == w })
		q.mu.Unlock()
		q.release(job.ID)
	}
}

// SlotFreed wakes the longest-waiting job throttled by provider.
func (q *JobQueue) SlotFreed(provider string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := slices.IndexFunc(q.waiters, func(w *slotWaiter) bool { return slices.Contains(w.providers, provider) })
	if i < 0 {
		return
	}
	q.waiters[i].freedBy = provider
	close(q.waiters[i].wake)
	q.waiters = slices.Delete(q.waiters, i, i+1)
}

// shutdown refuses new jobs and wakes every worker; the closer task calls it
// when the group stops.
func (q *JobQueue) shutdown() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast() // Wake all waiting workers
	q.logger.Info().Msg("Job queue stopped")
}

// worker is the main loop for a single worker task.
func (q *JobQueue) worker(id int) {
	for {
		job := q.pop()
		if job == nil {
			q.logger.Debug().Int("worker_id", id).Msg("Worker exiting")
			return
		}

		q.logger.Debug().
			Int("worker_id", id).
			Str("job_id", job.ID).
			Str("type", string(job.Type)).
			Int("queued", q.Len()).
			Msg("Processing job")

		q.active.Add(1)
		q.runJob(job)
		q.active.Add(-1)
		q.release(job.ID)
	}
}

// runJob executes a single job, recovering from panics so that one bad job
// cannot permanently kill a worker goroutine. With a fixed worker pool, an
// unrecovered panic per worker silently drained the pool to zero — leaving
// every queued download stuck at 0% while the healthcheck still passed.
func (q *JobQueue) runJob(job *Job) {
	defer func() {
		if r := recover(); r != nil {
			q.logger.Error().
				Str("job_id", job.ID).
				Str("type", string(job.Type)).
				Interface("panic", r).
				Bytes("stack", debug.Stack()).
				Msg("Recovered from panic while processing job")
		}
	}()
	q.processFunc(q.group.Context(), job)
}

// pop removes and returns the next job, blocking if queue is empty.
// Returns nil if the queue is closed and empty.
func (q *JobQueue) pop() *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.jobs) == 0 && !q.closed {
		q.cond.Wait()
	}

	if q.closed {
		return nil
	}

	job := q.jobs[0]
	q.jobs = q.jobs[1:]
	return job
}

// DeleteJob removes a pending job by ID (before it's picked up by a worker).
// Returns true if the job was found and removed.
func (q *JobQueue) DeleteJob(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, job := range q.jobs {
		if job.ID == jobID {
			q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
			q.dropHold(jobID)
			return true
		}
	}
	return false
}

// FindJob returns a pending job by ID without removing it.
func (q *JobQueue) FindJob(jobID string) *Job {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, job := range q.jobs {
		if job.ID == jobID {
			return job
		}
	}
	return nil
}

// PendingCount returns the count of pending jobs, optionally filtered by type.
func (q *JobQueue) PendingCount(jobType JobType) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if jobType == "" {
		return len(q.jobs)
	}

	count := 0
	for _, job := range q.jobs {
		if job.Type == jobType {
			count++
		}
	}
	return count
}

// Holds reports whether the queue will still run a job with this ID: pending, waiting for a slot or running.
func (q *JobQueue) Holds(jobID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.held[jobID] > 0
}

func (q *JobQueue) release(jobID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropHold(jobID)
}

// dropHold needs q.mu.
func (q *JobQueue) dropHold(jobID string) {
	if q.held[jobID]--; q.held[jobID] <= 0 {
		delete(q.held, jobID)
	}
}
