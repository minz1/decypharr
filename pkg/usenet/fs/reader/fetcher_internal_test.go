package reader

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

// newTestFetcher builds a cache+fetcher pair with no NNTP client. Its
// scheduler's only worker is parked, so hints stay queued without using the
// nil client.
func newTestFetcher(t *testing.T, segCount int) *SegmentFetcher {
	t.Helper()
	sf, _ := newTestFetcherOn(t, segCount, newParkedScheduler(t))
	return sf
}

// newTestFetcherOn builds a cache and a fetcher on scheduler, and returns the
// fetcher's group. The test stops the group before the cache closes.
func newTestFetcherOn(t *testing.T, segCount int, scheduler *FetchScheduler) (*SegmentFetcher, *task.Group) {
	t.Helper()
	const segSize = int64(1000)
	segs := make([]SegmentMeta, segCount)
	for i := range segs {
		segs[i] = SegmentMeta{
			MessageID:   fmt.Sprintf("<seg%d@test>", i),
			Number:      i + 1,
			Bytes:       segSize,
			StartOffset: int64(i) * segSize,
			EndOffset:   int64(i+1)*segSize - 1,
		}
	}
	cfg := DefaultConfig()
	cfg.DiskPath = t.TempDir()
	cfg.MaxConnections = 1
	cfg.Scheduler = scheduler

	stats := &Stats{}
	cache, err := newTestSegmentCache(t, segs, cfg, stats, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewSegmentCache: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	fetcherGroup := newTestRoot(t)
	return NewSegmentFetcher(fetcherGroup, nil, cache, cfg, stats, zerolog.Nop()), fetcherGroup
}

func TestCancelPendingPrefetchDrainsQueue(t *testing.T) {
	t.Parallel()
	sf := newTestFetcher(t, 10)

	for i := 2; i <= 5; i++ {
		sf.QueuePrefetch(i)
	}
	if got := pendingPrefetch(sf); got != 4 {
		t.Fatalf("expected 4 queued hints, got %d", got)
	}

	sf.CancelPendingPrefetch()

	if got := pendingPrefetch(sf); got != 0 {
		t.Errorf("expected empty queue after cancel, got %d hints", got)
	}
	if got := sf.stats.PrefetchCancelled.Load(); got != 4 {
		t.Errorf("PrefetchCancelled = %d, want 4", got)
	}

	// The dedup bits must be cleared so the same segments can be re-hinted
	// for the new window.
	sf.QueuePrefetch(3)
	if got := pendingPrefetch(sf); got != 1 {
		t.Errorf("expected segment re-queueable after cancel, queue len = %d", got)
	}
}

func TestPrefetchRangePipelinesAndPublishesEverySegment(t *testing.T) {
	t.Parallel()
	const depth = 4
	srv, cache, fetcher, stats := newPipelineTestFetcher(t, depth, func(int) bool { return true })

	fetcher.QueuePrefetchRange(0, depth-1)
	waitForSegmentState(t, cache, 0, StateOnDisk)
	waitForSegmentState(t, cache, depth-1, StateOnDisk)
	waitForCondition(t, func() bool { return stats.Downloads.Load() == depth })

	if got := srv.Bodies.Load(); got != depth {
		t.Fatalf("BODY responses = %d, want %d", got, depth)
	}
	if got := stats.Downloads.Load(); got != depth {
		t.Fatalf("completed downloads = %d, want %d", got, depth)
	}
}

func TestPrefetchRangePublishesSuccessAfterMissingArticle(t *testing.T) {
	t.Parallel()
	const depth = 2
	_, cache, fetcher, stats := newPipelineTestFetcher(t, depth, func(i int) bool { return i == 1 })

	fetcher.QueuePrefetchRange(0, depth-1)
	waitForSegmentState(t, cache, 0, StateFailed)
	waitForSegmentState(t, cache, 1, StateOnDisk)
	waitForCondition(t, func() bool { return stats.Downloads.Load()+stats.DownloadErrors.Load() == depth })

	if err := cache.GetError(0); !nntp.IsArticleNotFoundError(err) {
		t.Fatalf("missing segment error = %v, want article-not-found", err)
	}
	if got := stats.Downloads.Load(); got != 1 {
		t.Fatalf("completed downloads = %d, want 1", got)
	}
	if got := stats.DownloadErrors.Load(); got != 1 {
		t.Fatalf("download errors = %d, want 1", got)
	}
}

func TestStreamBodyPipelinePlanPreservesParallelism(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		workers      int
		segmentCount int
		priority     fetchPriority
		depth        int
		wantSingles  int
	}{
		{"depth one disables pipelining", 8, 16, priorityPrefetch, 1, 16},
		{"single worker", 1, 16, priorityPrefetch, 4, 16},
		{"short range preserves parallelism", 8, 8, priorityPrefetch, 2, 8},
		{"pipeline tail after filling workers", 8, 10, priorityPrefetch, 2, 7},
		{"full multi-worker pipeline", 8, 14, priorityPrefetch, 2, 0},
		{"two workers pipeline", 2, 2, priorityPrefetch, 2, 0},
		{"four deep tail after filling workers", 8, 10, priorityPrefetch, 4, 7},
		{"four deep full pipeline", 8, 28, priorityPrefetch, 4, 0},
		{"probe may use reserved worker", 8, 9, priorityProbe, 2, 9},
		{"probe pipeline tail", 8, 12, priorityProbe, 2, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sf := &SegmentFetcher{
				config:    Config{BodyPipelineDepth: tt.depth},
				scheduler: &FetchScheduler{workers: tt.workers},
			}
			if got := sf.streamBodyPipelineSingleCount(tt.segmentCount, tt.priority); got != tt.wantSingles {
				t.Fatalf("single articles = %d, want %d", got, tt.wantSingles)
			}
		})
	}
}

func newPipelineTestFetcher(
	t *testing.T,
	depth int,
	present func(int) bool,
) (*nntpd.Server, *SegmentCache, *SegmentFetcher, *Stats) {
	t.Helper()
	srv, cache, fetcher, stats, _ := newPipelineStack(t, depth, 2, present)
	return srv, cache, fetcher, stats
}

// newPipelineStack builds a fetcher over a real nntpd server with a shared
// scheduler of the given width.
func newPipelineStack(
	t *testing.T,
	depth, workers int,
	present func(int) bool,
) (*nntpd.Server, *SegmentCache, *SegmentFetcher, *Stats, *task.Group) {
	t.Helper()
	srv, err := nntpd.New(nntpd.Config{RTT: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	const segmentSize = 64 * 1024
	segments := make([]SegmentMeta, depth)
	for i := range segments {
		messageID := fmt.Sprintf("<pipeline-%d@nntpd>", i)
		offset := int64(i * segmentSize)
		if present(i) {
			srv.AddArticle(
				messageID,
				nntpd.Encode(nntpd.Pattern(offset, segmentSize), "pipeline.bin", i+1, int64(depth)*segmentSize, offset),
			)
		}
		segments[i] = SegmentMeta{
			MessageID:   messageID,
			Number:      i + 1,
			Bytes:       segmentSize,
			StartOffset: offset,
			EndOffset:   offset + segmentSize - 1,
		}
	}
	host, port := srv.Addr()
	nntpCfg := &config.Config{Usenet: config.Usenet{Providers: []config.UsenetProvider{{
		Host: host, Port: port, MaxConnections: 2,
	}}}}
	client, err := nntp.NewClient(newClientGroup(t), nntpCfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.DiskPath = t.TempDir()
	cfg.MaxConnections = 2
	cfg.BodyPipelineDepth = depth
	cfg.DownloadTimeout = 5 * time.Second
	stats := &Stats{}
	cache, err := newTestSegmentCache(t, segments, cfg, stats, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	root := newTestRoot(t)
	scheduler, _, _ := newTestScheduler(t, root, workers)
	cfg.Scheduler = scheduler
	fetcher := NewSegmentFetcher(root.Child("fetcher"), client, cache, cfg, stats, zerolog.Nop())
	return srv, cache, fetcher, stats, root
}

func waitForSegmentState(t *testing.T, cache *SegmentCache, segIdx int, want SegmentState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if state := cache.GetState(segIdx); state == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("segment %d state = %s, want %s (error: %v)", segIdx, cache.GetState(segIdx), want, cache.GetError(segIdx))
}

func waitForCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func TestEnsureSegmentsPropagatesPermanentFailure(t *testing.T) {
	t.Parallel()
	// The segments are failed already, so the fetch never reaches the nil client.
	scheduler, _, _ := newTestScheduler(t, newTestRoot(t), 1)
	sf, _ := newTestFetcherOn(t, 6, scheduler)

	notFound := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Message: "gone"}
	// Multiple missing segments exercises the concurrent fan-out path.
	for i := 1; i <= 4; i++ {
		sf.cache.MarkFetching(i)
		sf.cache.MarkFailed(i, notFound)
	}

	err := sf.EnsureSegments(context.Background(), 1, 4)
	if err == nil {
		t.Fatal("expected error for permanently failed segments")
	}
	if !nntp.IsArticleNotFoundError(err) {
		t.Errorf("expected article-not-found error, got %v", err)
	}
}

func TestSeekAbandonedWindow(t *testing.T) {
	t.Parallel()
	const ahead = 40
	cases := []struct {
		name             string
		prevEnd          int64
		startSeg, endSeg int
		want             bool
	}{
		{"first read never seeks", -1, 500, 501, false},
		{"sequential next read", 10, 11, 12, false},
		{"read within ahead window", 10, 45, 46, false},
		{"jump past ahead window", 10, 51, 52, true},
		{"small backward overlap", 50, 48, 49, false},
		{"backward within window", 50, 15, 16, false},
		{"backward seek past window", 100, 10, 11, true},
		{"prefetch disabled", 10, 500, 501, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := ahead
			if tc.name == "prefetch disabled" {
				a = 0
			}
			if got := seekAbandonedWindow(tc.prevEnd, tc.startSeg, tc.endSeg, a); got != tc.want {
				t.Errorf("seekAbandonedWindow(%d, %d, %d, %d) = %v, want %v",
					tc.prevEnd, tc.startSeg, tc.endSeg, a, got, tc.want)
			}
		})
	}
}

// TestFetcherStopDoesNotWaitForQueued proves stopping a fetcher's group does
// not wait behind another owner's closure holding the shared worker: the
// fetcher's queued closure is dropped, unrun, once the worker reaches it.
func TestFetcherStopDoesNotWaitForQueued(t *testing.T) {
	t.Parallel()
	root := newTestRoot(t)
	scheduler, _, other := newTestScheduler(t, root, 1)
	busy := make(chan struct{})
	busyDone := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	if !scheduler.submit(t.Context(), other, priorityDemand, func() {
		close(busy)
		<-release
		close(busyDone)
	}, nil) {
		t.Fatal("submit the busy closure")
	}
	<-busy

	sf, fetcherGroup := newTestFetcherOn(t, 4, scheduler)
	var ran atomic.Bool
	dropped := make(chan struct{})
	if !sf.submit(t.Context(), priorityPrefetch, func() { ran.Store(true) }, func() { close(dropped) }) {
		t.Fatal("queue a prefetch")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
	defer cancel()
	if err := fetcherGroup.Stop(ctx); err != nil {
		t.Fatalf("fetcher Stop: %v", err)
	}
	select {
	case <-busyDone:
		t.Fatal("the busy closure ended before the fetcher stopped")
	default:
	}

	releaseOnce()
	<-dropped
	if ran.Load() {
		t.Error("the queued closure ran after its fetcher stopped")
	}
	if err := root.Stop(ctx); err != nil {
		t.Fatalf("root Stop: %v", err)
	}
	if snap := append(root.Snapshot(), fetcherGroup.Snapshot()...); len(snap) != 0 {
		t.Errorf("tasks left: %v", snap)
	}
}

// TestWorkerSurvivesPanickingClosure proves a panic inside a fetch closure is
// contained: the fetch reports a failure, the single worker runs the next
// closure, and the panicked segment can be fetched again.
func TestWorkerSurvivesPanickingClosure(t *testing.T) {
	t.Parallel()
	_, cache, sf, _, root := newPipelineStack(t, 2, 1, func(int) bool { return true })
	var calls atomic.Int32
	sf.afterMarkFetchingHook = func() {
		if calls.Add(1) == 1 {
			panic("injected fetch panic")
		}
	}

	if err := sf.Fetch(t.Context(), 0); !errors.Is(err, ErrCacheClosed) {
		t.Fatalf("Fetch after a panic = %v, want the dropped-closure error", err)
	}
	if state := cache.GetState(0); state != StateEmpty {
		t.Fatalf("panicked segment state = %s, want %s", state, StateEmpty)
	}
	ran := make(chan struct{})
	if !sf.submit(t.Context(), priorityDemand, func() { close(ran) }, nil) {
		t.Fatal("submit the follow-up closure")
	}
	<-ran
	if err := sf.Fetch(t.Context(), 0); err != nil {
		t.Fatalf("Fetch of the panicked segment: %v", err)
	}
	if state := cache.GetState(0); state != StateOnDisk {
		t.Fatalf("segment state = %s, want %s", state, StateOnDisk)
	}
	if err := root.Stop(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if snap := root.Snapshot(); len(snap) != 0 {
		t.Errorf("tasks left: %v", snap)
	}
}
