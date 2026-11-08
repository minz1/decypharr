package reader

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/sirrobot01/decypharr/internal/task"
)

type fetchPriority uint8

const (
	priorityPrefetch fetchPriority = iota
	priorityProbe
	priorityDemand
)

// scheduledFetch is one queued closure. owner is the group whose lease the
// closure runs under; drop settles the closure when it never runs.
type scheduledFetch struct {
	owner *task.Group
	run   func()
	drop  func()
}

// Queue depths: a floor plus room per worker.
const (
	minDemandQueue         = 64
	demandQueuePerWorker   = 16
	minPrefetchQueue       = 256
	prefetchQueuePerWorker = 32
)

// errSchedulerRequired is returned by NewStreamingReader without a shared
// scheduler: readers do not own workers.
var errSchedulerRequired = errors.New("streaming reader needs the service's fetch scheduler")

// errFetchClosurePanicked reports a scheduled closure that panicked; the
// scheduler's group logs it with the stack.
var errFetchClosurePanicked = errors.New("scheduled fetch closure panicked")

// FetchScheduler bounds NNTP work across every reader sharing a client.
// Foreground reads use the demand queue; speculative work uses prefetch. Its
// workers are tasks of the group it was made on; the group's owner stops it.
type FetchScheduler struct {
	g        *task.Group
	mu       sync.RWMutex // submitters hold RLock; stopping takes Lock
	closed   bool
	demand   chan scheduledFetch
	prefetch chan scheduledFetch
	workers  int
}

// NewFetchScheduler starts workers tasks on g, matching the real provider
// concurrency. It returns an error if workers < 1 or g refuses tasks.
func NewFetchScheduler(g *task.Group, workers int) (*FetchScheduler, error) {
	if workers < 1 {
		return nil, fmt.Errorf("fetch scheduler needs at least one worker, got %d", workers)
	}
	s := &FetchScheduler{
		g:        g,
		demand:   make(chan scheduledFetch, max(minDemandQueue, workers*demandQueuePerWorker)),
		prefetch: make(chan scheduledFetch, max(minPrefetchQueue, workers*prefetchQueuePerWorker)),
		workers:  workers,
	}
	reserved := 0
	if workers > 1 {
		reserved = 1
	}
	for i := range workers {
		allowPrefetch := i >= reserved
		if err := g.Go("worker", func(ctx context.Context) error {
			return s.worker(ctx, allowPrefetch)
		}); err != nil {
			return nil, fmt.Errorf("start fetch worker: %w", err)
		}
	}
	return s, nil
}

// worker runs closures until ctx ends, then refuses new work and drops what
// is still queued.
func (s *FetchScheduler) worker(ctx context.Context, allowPrefetch bool) error {
	defer s.stop()
	// A worker reserved for demand never receives from the nil channel.
	prefetch := s.prefetch
	if !allowPrefetch {
		prefetch = nil
	}
	for ctx.Err() == nil {
		if allowPrefetch {
			// Prefer demand already waiting before accepting speculative work.
			select {
			case f := <-s.demand:
				s.execute(ctx, f)
				continue
			default:
			}
		}
		select {
		case <-ctx.Done():
		case f := <-s.demand:
			s.execute(ctx, f)
		case f := <-prefetch:
			s.execute(ctx, f)
		}
	}
	return nil
}

// execute runs f under a lease on its owner, or drops it when the scheduler
// or the owner has stopped. A panic is logged with its stack and the closure
// is dropped, so the worker survives.
func (s *FetchScheduler) execute(ctx context.Context, f scheduledFetch) {
	if ctx.Err() != nil {
		f.settle()
		return
	}
	leave, err := f.owner.Enter("fetch")
	if err != nil {
		f.settle()
		return
	}
	defer leave()
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := debug.Stack()
			failure := fmt.Errorf("%w: %v\n%s", errFetchClosurePanicked, recovered, stack)
			// The group logs a task's error; if it is stopping the log is lost.
			_ = s.g.Go("fetch-panic", func(context.Context) error { return failure })
			f.settle()
		}
	}()
	f.run()
}

func (f scheduledFetch) settle() {
	if f.drop != nil {
		f.drop()
	}
}

// stop refuses further submissions and drops everything still queued.
func (s *FetchScheduler) stop() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	for {
		select {
		case f := <-s.demand:
			f.settle()
		case f := <-s.prefetch:
			f.settle()
		default:
			return
		}
	}
}

// submit queues run on behalf of owner. It reports false, without calling
// drop, when the closure was not accepted. An accepted closure is settled
// exactly once: run, or drop if it never runs.
func (s *FetchScheduler) submit(
	ctx context.Context,
	owner *task.Group,
	priority fetchPriority,
	run, drop func(),
) bool {
	if s == nil || run == nil || owner == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	f := scheduledFetch{owner: owner, run: run, drop: drop}
	if priority != priorityDemand {
		// Speculative work never waits for room.
		queue := s.prefetch
		if priority == priorityProbe {
			queue = s.demand
		}
		select {
		case queue <- f:
			return true
		default:
			return false
		}
	}
	select {
	case s.demand <- f:
		return true
	case <-ctx.Done():
		return false
	case <-owner.Context().Done():
		return false
	case <-s.g.Context().Done():
		return false
	}
}
