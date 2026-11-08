package reader

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

// newTestRoot makes a root group that the test stops on cleanup.
func newTestRoot(t testing.TB) *task.Group {
	t.Helper()
	root := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() { stopTestGroup(t, root) })
	return root
}

// newTestScheduler starts a scheduler with the given workers on a child of
// root, and returns it with its group and a separate owner group that
// closures run under. root stops the owner last, after the scheduler.
func newTestScheduler(t testing.TB, root *task.Group, workers int) (*FetchScheduler, *task.Group, *task.Group) {
	t.Helper()
	owner := root.Child("owner")
	schedGroup := root.Child("scheduler")
	scheduler, err := NewFetchScheduler(schedGroup, workers)
	if err != nil {
		t.Fatalf("NewFetchScheduler: %v", err)
	}
	return scheduler, schedGroup, owner
}

// newParkedScheduler returns a one-worker scheduler whose worker is parked in
// a closure until the test ends, so submitted work stays queued and never
// reaches the (absent) NNTP client.
func newParkedScheduler(t testing.TB) *FetchScheduler {
	t.Helper()
	root := newTestRoot(t)
	scheduler, _, owner := newTestScheduler(t, root, 1)
	parked := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	if !scheduler.submit(t.Context(), owner, priorityDemand, func() {
		close(parked)
		<-release
	}, nil) {
		t.Fatal("submit the parking closure")
	}
	<-parked
	return scheduler
}

func TestNewFetchSchedulerNeedsWorkers(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t)
	if _, err := NewFetchScheduler(root.Child("scheduler"), 0); err == nil {
		t.Fatal("NewFetchScheduler(0) succeeded")
	}
}

func TestFetchSchedulerRunsDemandBeforeQueuedPrefetch(t *testing.T) {
	t.Parallel()
	scheduler, _, owner := newTestScheduler(t, newTestRoot(t), 1)

	started := make(chan struct{})
	release := make(chan struct{})
	order := make(chan string, 3)
	if !scheduler.submit(context.Background(), owner, priorityPrefetch, func() {
		close(started)
		<-release
		order <- "active-prefetch"
	}, nil) {
		t.Fatal("submit active prefetch")
	}
	<-started
	if !scheduler.submit(context.Background(), owner, priorityPrefetch, func() { order <- "queued-prefetch" }, nil) {
		t.Fatal("submit queued prefetch")
	}
	if !scheduler.submit(context.Background(), owner, priorityDemand, func() { order <- "demand" }, nil) {
		t.Fatal("submit demand")
	}
	close(release)

	got := []string{<-order, <-order, <-order}
	want := []string{"active-prefetch", "demand", "queued-prefetch"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("execution order = %v, want %v", got, want)
		}
	}
}

func TestFetchSchedulerBoundsConcurrency(t *testing.T) {
	t.Parallel()
	const workers = 3
	scheduler, _, owner := newTestScheduler(t, newTestRoot(t), workers)

	var active, peak atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		if !scheduler.submit(context.Background(), owner, priorityDemand, func() {
			n := active.Add(1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			wg.Done()
		}, func() { wg.Done() }) {
			t.Fatal("submit demand")
		}
	}
	for range workers {
		<-started
	}
	close(release)
	wg.Wait()
	if got := peak.Load(); got != workers {
		t.Fatalf("peak concurrency = %d, want %d", got, workers)
	}
}

func TestFetchSchedulerStopSettlesAcceptedTasks(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t)
	scheduler, _, owner := newTestScheduler(t, root, 2)
	var accepted, settled atomic.Int64
	start := make(chan struct{})
	var submitters sync.WaitGroup
	for range 256 {
		submitters.Go(func() {
			<-start
			if scheduler.submit(context.Background(), owner, priorityPrefetch,
				func() { settled.Add(1) }, func() { settled.Add(1) }) {
				accepted.Add(1)
			}
		})
	}
	close(start)
	stopped := make(chan error, 1)
	go func() { stopped <- root.Stop(context.WithoutCancel(t.Context())) }()
	submitters.Wait()
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got, want := settled.Load(), accepted.Load(); got != want {
		t.Fatalf("settled tasks = %d, accepted = %d", got, want)
	}
}

// TestSchedulerStopWithBlockedDemandSubmit proves a demand submit blocked on a
// full queue no longer wedges the stop: it leaves when the scheduler's group
// is cancelled, and the whole tree stops within its budget.
func TestSchedulerStopWithBlockedDemandSubmit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		root := task.NewRoot(t.Context(), "test", zerolog.Nop())
		scheduler, schedGroup, owner := newTestScheduler(t, root, 1)

		started := make(chan struct{})
		if !scheduler.submit(t.Context(), owner, priorityDemand, func() {
			close(started)
			<-schedGroup.Context().Done()
		}, nil) {
			t.Fatal("submit the blocking closure")
		}
		<-started
		var dropped atomic.Int64
		for range cap(scheduler.demand) {
			if !scheduler.submit(t.Context(), owner, priorityDemand,
				func() { t.Error("a queued closure ran") }, func() { dropped.Add(1) }) {
				t.Fatal("fill the demand queue")
			}
		}
		blocked := make(chan bool, 1)
		go func() {
			blocked <- scheduler.submit(t.Context(), owner, priorityDemand, func() {}, nil)
		}()
		synctest.Wait()

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := root.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if <-blocked {
			t.Error("the blocked submit was accepted")
		}
		if got, want := dropped.Load(), int64(cap(scheduler.demand)); got != want {
			t.Errorf("dropped = %d, want %d", got, want)
		}
		if snap := root.Snapshot(); len(snap) != 0 {
			t.Errorf("tasks left: %v", snap)
		}
	})
}

// TestSchedulerRushedStopDropsQueued proves a stop whose budget is already
// spent still reaches Done and drops every queued closure unrun.
func TestSchedulerRushedStopDropsQueued(t *testing.T) {
	t.Parallel()
	root := task.NewRoot(t.Context(), "test", zerolog.Nop())
	scheduler, schedGroup, owner := newTestScheduler(t, root, 1)

	started := make(chan struct{})
	if !scheduler.submit(t.Context(), owner, priorityDemand, func() {
		close(started)
		<-schedGroup.Context().Done()
	}, nil) {
		t.Fatal("submit the blocking closure")
	}
	<-started
	const queued = 10
	var dropped, ran atomic.Int64
	for i := range queued {
		priority := priorityPrefetch
		if i%2 == 0 {
			priority = priorityDemand
		}
		if !scheduler.submit(t.Context(), owner, priority,
			func() { ran.Add(1) }, func() { dropped.Add(1) }) {
			t.Fatal("queue a closure")
		}
	}

	expired, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	cancel()
	_ = root.Stop(expired) // may report the rush; Done is what matters
	select {
	case <-root.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the root did not reach Done after a rushed stop")
	}
	if got := dropped.Load(); got != queued {
		t.Errorf("dropped = %d, want %d", got, queued)
	}
	if got := ran.Load(); got != 0 {
		t.Errorf("%d queued closures ran after the stop", got)
	}
	if snap := root.Snapshot(); len(snap) != 0 {
		t.Errorf("tasks left: %v", snap)
	}
}

// TestCloseWhileSubmittingRace stops the scheduler while submitters hammer it:
// every accepted closure is settled, and later submits are refused.
func TestCloseWhileSubmittingRace(t *testing.T) {
	t.Parallel()
	root := task.NewRoot(t.Context(), "test", zerolog.Nop())
	scheduler, schedGroup, owner := newTestScheduler(t, root, 2)

	var accepted, settled atomic.Int64
	var submitters sync.WaitGroup
	for i := range 8 {
		submitters.Go(func() {
			priority := fetchPriority(i % 3)
			for schedGroup.Context().Err() == nil {
				if scheduler.submit(context.Background(), owner, priority,
					func() { settled.Add(1) }, func() { settled.Add(1) }) {
					accepted.Add(1)
				}
			}
		})
	}
	stopped := make(chan error, 1)
	go func() { stopped <- root.Stop(context.WithoutCancel(t.Context())) }()
	submitters.Wait()
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got, want := settled.Load(), accepted.Load(); got != want {
		t.Fatalf("settled = %d, accepted = %d", got, want)
	}
	if scheduler.submit(context.Background(), owner, priorityDemand, func() {}, nil) {
		t.Error("submit after stop was accepted")
	}
	if snap := root.Snapshot(); len(snap) != 0 {
		t.Errorf("tasks left: %v", snap)
	}
}
