package reader

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	appconfig "github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

// SegmentFetcher deduplicates downloads and submits them to the shared scheduler.
type SegmentFetcher struct {
	client *nntp.Client
	cache  *SegmentCache
	config Config
	logger zerolog.Logger
	stats  *Stats

	// Request deduplication
	inFlight   map[int]*fetchPromise
	inFlightMu sync.Mutex

	// Background prefetch
	scheduler      *FetchScheduler
	ownedScheduler bool
	prefetchQueued []atomic.Uint64 // one deduplication bit per segment
	prefetchGen    atomic.Uint64
	taskMu         sync.Mutex
	taskWg         sync.WaitGroup
	closing        bool

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
}

// fetchPromise allows multiple goroutines to wait for the same segment download.
type fetchPromise struct {
	done chan struct{}
	err  error
}

type prefetchClaim struct {
	segIdx  int
	promise *fetchPromise
}

// NewSegmentFetcher creates a new segment fetcher.
func NewSegmentFetcher(
	ctx context.Context,
	client *nntp.Client,
	cache *SegmentCache,
	config Config,
	stats *Stats,
	logger zerolog.Logger,
) *SegmentFetcher {
	ctx, cancel := context.WithCancel(ctx)
	config.BodyPipelineDepth = appconfig.NormalizeBodyPipelineDepth(config.BodyPipelineDepth)

	maxConns := config.MaxConnections
	if maxConns < 1 {
		maxConns = defaultMaxConnections
	}

	sf := &SegmentFetcher{
		client:   client,
		cache:    cache,
		config:   config,
		logger:   logger.With().Str("component", "fetcher").Logger(),
		stats:    stats,
		inFlight: make(map[int]*fetchPromise),
		// A packed bitmap avoids per-segment goroutines and large bool arrays.
		prefetchQueued: make([]atomic.Uint64, (cache.SegmentCount()+bitsPerWord-1)/bitsPerWord),
		ctx:            ctx,
		cancel:         cancel,
	}
	sf.scheduler = config.Scheduler
	if sf.scheduler == nil {
		workers := maxConns
		if client == nil {
			workers = 0
		}
		sf.scheduler = NewFetchScheduler(workers)
		sf.ownedScheduler = true
	}

	return sf
}

// Fetch schedules a foreground download and waits for it. Provider concurrency
// is shared across readers, so opening more files cannot multiply workers.
func (sf *SegmentFetcher) Fetch(ctx context.Context, segIdx int) error {
	return sf.scheduleAndWait(ctx, priorityDemand, func() error {
		return sf.fetchWithRetryDirect(sf.ctx, segIdx, nntp.WorkloadStreamDemand)
	})
}

func (sf *SegmentFetcher) scheduleAndWait(ctx context.Context, priority fetchPriority, run func() error) error {
	return sf.waitScheduled(ctx, sf.schedule(ctx, priority, run))
}

func (sf *SegmentFetcher) schedule(ctx context.Context, priority fetchPriority, run func() error) <-chan error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	if sf.scheduler.workers == 0 && priority == priorityDemand {
		done <- run()
		return done
	}
	if !sf.submit(ctx, priority, func() { done <- run() }, func() { done <- ErrCacheClosed }) {
		if err := ctx.Err(); err != nil {
			done <- err
		} else if ctxErr := sf.ctx.Err(); ctxErr != nil {
			done <- ctxErr
		} else {
			done <- ErrCacheClosed
		}
	}
	return done
}

func (sf *SegmentFetcher) waitScheduled(ctx context.Context, done <-chan error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-sf.ctx.Done():
		return sf.ctx.Err()
	}
}

func (sf *SegmentFetcher) submit(ctx context.Context, priority fetchPriority, run, drop func()) bool {
	sf.taskMu.Lock()
	if sf.closing {
		sf.taskMu.Unlock()
		return false
	}
	sf.taskWg.Add(1)
	sf.taskMu.Unlock()

	if sf.scheduler.submit(ctx, priority, func() {
		defer sf.taskWg.Done()
		run()
	}, func() {
		defer sf.taskWg.Done()
		if drop != nil {
			drop()
		}
	}) {
		return true
	}
	sf.taskWg.Done()
	return false
}

// fetchDirect performs one deduplicated fetch inside a scheduler worker.
func (sf *SegmentFetcher) fetchDirect(ctx context.Context, segIdx int, workload nntp.Workload) error {
	// Fast path: already cached, or wait until an extent eviction completes.
	for ready := false; !ready; {
		switch sf.cache.GetState(segIdx) {
		case StateEvicting:
			if err := sf.cache.WaitForEvictionRelease(ctx, segIdx); err != nil {
				return err
			}
			// slot is Empty now; re-evaluate
		case StateOnDisk:
			return nil
		case StateFailed:
			return sf.cache.GetError(segIdx)
		case StateEmpty, StateFetching:
			ready = true
		}
	}

	// Check if someone else is already fetching
	sf.inFlightMu.Lock()
	if promise, ok := sf.inFlight[segIdx]; ok {
		sf.inFlightMu.Unlock()
		// Wait for existing fetch
		select {
		case <-promise.done:
			return promise.err
		case <-ctx.Done():
			return ctx.Err()
		case <-sf.ctx.Done():
			return sf.ctx.Err()
		}
	}

	// We're the first - create promise
	promise := &fetchPromise{done: make(chan struct{})}
	sf.inFlight[segIdx] = promise
	sf.inFlightMu.Unlock()

	// Actually fetch
	err := sf.doFetch(ctx, segIdx, workload)
	promise.err = err
	close(promise.done)

	// Cleanup
	sf.inFlightMu.Lock()
	delete(sf.inFlight, segIdx)
	sf.inFlightMu.Unlock()

	return err
}

// doFetch performs the actual NNTP download.
func (sf *SegmentFetcher) doFetch(ctx context.Context, segIdx int, workload nntp.Workload) error {
	return sf.doFetchAttempt(ctx, segIdx, workload, 0)
}

// doFetchRestarts bounds how many times a single doFetch may restart because
// another party held the slot and then lost it (a cancelled fetch, or an
// eviction landing between the state check and the claim). Each restart does
// real work, so this only stops a pathological loop.
const doFetchRestarts = 4

func (sf *SegmentFetcher) doFetchAttempt(ctx context.Context, segIdx int, workload nntp.Workload, restarts int) error {
	seg := sf.cache.GetSegment(segIdx)
	if seg == nil {
		return ErrSegmentNotFound
	}
	if restarts > doFetchRestarts {
		return fmt.Errorf("segment %d: slot contended after %d restarts", segIdx, restarts)
	}

	// Try to mark as fetching (atomic transition Empty -> Fetching)
	if !sf.cache.MarkFetching(segIdx) {
		return sf.awaitOtherFetch(ctx, segIdx, workload, restarts)
	}

	downloadCtx, cancel := context.WithTimeout(ctx, sf.downloadTimeout())
	defer cancel()

	// ExecuteWithFailover already retries across configured providers.
	err := sf.client.ExecuteWithFailover(downloadCtx, workload, func(conn *nntp.Connection) error {
		return sf.downloadSegment(downloadCtx, conn, segIdx, seg.MessageID)
	})
	if err != nil {
		sf.stats.DownloadErrors.Add(1)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			sf.cache.ReleaseFetching(segIdx)
			return err
		}
		sf.cache.MarkFailed(segIdx, err)
		return err
	}

	sf.stats.Downloads.Add(1)
	return nil
}

// awaitOtherFetch handles a lost Empty->Fetching claim: the segment is
// cached, failed, or owned by another fetch or eviction.
func (sf *SegmentFetcher) awaitOtherFetch(ctx context.Context, segIdx int, workload nntp.Workload, restarts int) error {
	switch sf.cache.GetState(segIdx) {
	case StateOnDisk:
		return nil
	case StateFailed:
		return sf.cache.GetError(segIdx)
	case StateFetching:
		// Wait for the other fetcher. If it released the slot (cancel)
		// or the segment was dropped right after it landed, the wait
		// reports ErrSegmentEvicted rather than blocking on an event
		// that is no longer coming — retry the fetch ourselves.
		err := sf.cache.WaitForSegment(ctx, segIdx)
		if errors.Is(err, ErrSegmentEvicted) {
			return sf.doFetchAttempt(ctx, segIdx, workload, restarts+1)
		}
		return err
	case StateEvicting:
		if err := sf.cache.WaitForEvictionRelease(ctx, segIdx); err != nil {
			return err
		}
		return sf.doFetchAttempt(ctx, segIdx, workload, restarts+1)
	case StateEmpty:
		// Freed between the claim attempt and the state read; claim again.
	}
	return sf.doFetchAttempt(ctx, segIdx, workload, restarts+1)
}

// defaultDownloadTimeout bounds one article download when unconfigured.
const defaultDownloadTimeout = 60 * time.Second

func (sf *SegmentFetcher) downloadTimeout() time.Duration {
	if sf.config.DownloadTimeout <= 0 {
		return defaultDownloadTimeout
	}
	return sf.config.DownloadTimeout
}

// downloadSegment decodes one article on conn straight into the cache. A
// cancelled download closes the connection to unblock the read.
func (sf *SegmentFetcher) downloadSegment(
	downloadCtx context.Context,
	conn *nntp.Connection,
	segIdx int,
	messageID string,
) error {
	stopCancel := context.AfterFunc(downloadCtx, func() {
		_ = conn.Close()
	})
	defer stopCancel()

	writer := sf.cache.StreamWriter(segIdx)
	if writer == nil {
		return ErrCacheClosed
	}

	var n int64
	var err error
	if sf.cache.memoryMode {
		var decoded []byte
		decoded, err = conn.DecodeBodyWithBuffer(messageID, writer)
		if err == nil {
			n, err = writer.Adopt(decoded)
		}
	} else {
		n, err = conn.StreamBody(messageID, writer)
	}
	if ctxErr := downloadCtx.Err(); ctxErr != nil {
		writer.Discard()
		return ctxErr
	}
	if err != nil {
		writer.Discard()
		return err
	}
	// Treat zero-byte articles as missing — the article exists on the
	// server but its body is empty/corrupted after yEnc decoding.
	if n == 0 {
		writer.Discard()
		return errNoDecodedData()
	}

	// Commit (updates cache state to StateOnDisk).
	writer.Finalize()
	return nil
}

func (sf *SegmentFetcher) markPrefetchQueued(segIdx int) bool {
	if segIdx < 0 || segIdx >= sf.cache.SegmentCount() {
		return false
	}
	word := &sf.prefetchQueued[segIdx/bitsPerWord]
	mask := uint64(1) << uint(segIdx%bitsPerWord)
	for {
		old := word.Load()
		if old&mask != 0 {
			return false
		}
		if word.CompareAndSwap(old, old|mask) {
			return true
		}
	}
}

func (sf *SegmentFetcher) clearPrefetchQueued(segIdx int) {
	if segIdx < 0 || segIdx >= sf.cache.SegmentCount() {
		return
	}
	word := &sf.prefetchQueued[segIdx/bitsPerWord]
	mask := uint64(1) << uint(segIdx%bitsPerWord)
	word.And(^mask)
}

// QueuePrefetch submits speculative work without creating a per-file worker.
func (sf *SegmentFetcher) QueuePrefetch(segIdx int) {
	sf.queueSpeculative(segIdx, priorityPrefetch)
}

func (sf *SegmentFetcher) QueueProbe(segIdx int) {
	if sf.scheduler.workers > 1 {
		sf.queueSpeculative(segIdx, priorityProbe)
	}
}

func (sf *SegmentFetcher) QueueProbeRange(startSeg, endSeg int) {
	if sf.scheduler.workers <= 1 {
		return
	}
	sf.queueSpeculativeRange(startSeg, endSeg, priorityProbe)
}

func (sf *SegmentFetcher) queueSpeculativeRange(startSeg, endSeg int, priority fetchPriority) {
	segmentCount := endSeg - startSeg + 1
	singleCount := sf.streamBodyPipelineSingleCount(segmentCount, priority)
	for segIdx := startSeg; segIdx < startSeg+singleCount; segIdx++ {
		sf.queueSpeculative(segIdx, priority)
	}
	depth := sf.config.BodyPipelineDepth
	for start := startSeg + singleCount; start <= endSeg; start += depth {
		sf.queueSpeculativeBatch(start, min(start+depth-1, endSeg), priority)
	}
}

func (sf *SegmentFetcher) streamBodyPipelineSingleCount(segmentCount int, priority fetchPriority) int {
	depth := sf.config.BodyPipelineDepth
	if segmentCount <= 1 || sf.scheduler.workers <= 1 || depth <= 1 {
		return max(segmentCount, 0)
	}
	availableWorkers := sf.scheduler.workers
	if priority == priorityPrefetch {
		availableWorkers-- // one scheduler worker is reserved for demand
	}
	if segmentCount >= availableWorkers*depth {
		return 0 // every worker can start with a pipeline
	}
	if segmentCount > availableWorkers+1 {
		// Fill every usable worker with a single article first, then pipeline
		// the tail without sacrificing initial connection parallelism.
		return availableWorkers
	}
	return segmentCount
}

func (sf *SegmentFetcher) queueSpeculative(segIdx int, priority fetchPriority) {
	state := sf.cache.GetState(segIdx)
	if state == StateOnDisk || state == StateFetching || !sf.markPrefetchQueued(segIdx) {
		return
	}
	sf.submitSpeculative(priority, []int{segIdx}, func() { sf.prefetchOne(segIdx) })
}

// submitSpeculative queues run for segments whose dedup bits are claimed. A
// task still queued when CancelPendingPrefetch bumps the generation becomes a
// no-op; the cancel already cleared its bits.
func (sf *SegmentFetcher) submitSpeculative(priority fetchPriority, segs []int, run func()) {
	gen := sf.prefetchGen.Load()
	release := func() {
		if gen == sf.prefetchGen.Load() {
			sf.clearPrefetchQueuedAll(segs)
		}
	}
	accepted := sf.submit(sf.ctx, priority, func() {
		if gen != sf.prefetchGen.Load() {
			return
		}
		defer release()
		run()
	}, release)
	if !accepted {
		sf.clearPrefetchQueuedAll(segs)
		sf.stats.PrefetchMisses.Add(int64(len(segs)))
	}
}

func (sf *SegmentFetcher) clearPrefetchQueuedAll(segs []int) {
	for _, segIdx := range segs {
		sf.clearPrefetchQueued(segIdx)
	}
}

func (sf *SegmentFetcher) prefetchOne(segIdx int) {
	if sf.cache.GetState(segIdx) == StateOnDisk {
		sf.stats.PrefetchHits.Add(1)
		return
	}
	fetchCtx, cancel := context.WithTimeout(sf.ctx, sf.downloadTimeout())
	err := sf.fetchWithRetryDirect(fetchCtx, segIdx, nntp.WorkloadStreamPrefetch)
	cancel()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		sf.logger.Debug().Err(err).Int("segment", segIdx).Msg("prefetch failed")
	}
}

func (sf *SegmentFetcher) queueSpeculativeBatch(startSeg, endSeg int, priority fetchPriority) {
	batch := make([]int, 0, appconfig.MaxBodyPipelineDepth)
	for segIdx := startSeg; segIdx <= endSeg; segIdx++ {
		state := sf.cache.GetState(segIdx)
		if state != StateOnDisk && state != StateFetching && sf.markPrefetchQueued(segIdx) {
			batch = append(batch, segIdx)
		}
	}
	if len(batch) > 0 {
		sf.submitSpeculative(priority, batch, func() { sf.prefetchBatch(batch) })
	}
}

// QueuePrefetchRange queues multiple segments for prefetch.
func (sf *SegmentFetcher) QueuePrefetchRange(startSeg, endSeg int) {
	sf.queueSpeculativeRange(startSeg, endSeg, priorityPrefetch)
}

func (sf *SegmentFetcher) claimPrefetch(segIdx int) (prefetchClaim, bool) {
	sf.inFlightMu.Lock()
	defer sf.inFlightMu.Unlock()
	if _, exists := sf.inFlight[segIdx]; exists {
		return prefetchClaim{}, false
	}
	if sf.cache.GetState(segIdx) == StateFailed {
		sf.cache.ResetFailed(segIdx)
	}
	if !sf.cache.MarkFetching(segIdx) {
		return prefetchClaim{}, false
	}
	promise := &fetchPromise{done: make(chan struct{})}
	sf.inFlight[segIdx] = promise
	return prefetchClaim{segIdx: segIdx, promise: promise}, true
}

func (sf *SegmentFetcher) finishPrefetchClaim(claim prefetchClaim, err error) {
	if err == nil {
		sf.stats.Downloads.Add(1)
	} else {
		sf.stats.DownloadErrors.Add(1)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			sf.cache.ReleaseFetching(claim.segIdx)
		} else {
			sf.cache.MarkFailed(claim.segIdx, err)
		}
	}
	claim.promise.err = err
	close(claim.promise.done)
	sf.inFlightMu.Lock()
	if sf.inFlight[claim.segIdx] == claim.promise {
		delete(sf.inFlight, claim.segIdx)
	}
	sf.inFlightMu.Unlock()
}

func (sf *SegmentFetcher) fetchPrefetchBatch(ctx context.Context, segIndices []int) error {
	pipeline := &prefetchPipeline{sf: sf}
	err := sf.client.ExecuteWithFailover(ctx, nntp.WorkloadStreamPrefetch, func(conn *nntp.Connection) error {
		if !pipeline.initialized {
			pipeline.initialized = true
			if claimErr := pipeline.claim(segIndices); claimErr != nil {
				return claimErr
			}
		}
		if len(pipeline.claims) == 0 {
			return nil
		}
		return pipeline.attempt(ctx, conn)
	})
	return pipeline.finish(err)
}

// prefetchPipeline is one pipelined BODY batch across provider failover.
// Claims and writers are made on the first attempt; a later attempt (another
// provider) re-requests the bodies but skips writers that already accepted
// one, so failover never overwrites accepted storage.
type prefetchPipeline struct {
	sf           *SegmentFetcher
	claims       []prefetchClaim
	writers      []SegmentWriter
	messageIDs   []string
	destinations []nntp.BodyDestination
	decoded      [][]byte
	written      []int64
	bodyErrors   []error
	initialized  bool
}

// claim takes ownership of every segment not cached or fetched elsewhere.
func (pp *prefetchPipeline) claim(segIndices []int) error {
	sf := pp.sf
	for _, segIdx := range segIndices {
		claim, ok := sf.claimPrefetch(segIdx)
		if !ok {
			if sf.cache.GetState(segIdx) == StateOnDisk {
				sf.stats.PrefetchHits.Add(1)
			}
			continue
		}
		pp.claims = append(pp.claims, claim)
		segment := sf.cache.GetSegment(segIdx)
		writer := sf.cache.StreamWriter(segIdx)
		if segment == nil || writer == nil {
			return ErrCacheClosed
		}
		pp.writers = append(pp.writers, writer)
		pp.messageIDs = append(pp.messageIDs, segment.MessageID)
		var destination nntp.BodyDestination
		if sf.cache.memoryMode {
			destination.BufferSource = writer
		} else {
			destination.Writer = writer
		}
		pp.destinations = append(pp.destinations, destination)
	}
	return nil
}

// attempt pipelines the bodies on one connection, closing it if ctx ends.
func (pp *prefetchPipeline) attempt(ctx context.Context, conn *nntp.Connection) error {
	if pp.bodyErrors == nil {
		pp.bodyErrors = make([]error, len(pp.claims))
		pp.decoded = make([][]byte, len(pp.claims))
		pp.written = make([]int64, len(pp.claims))
	}
	cancelFinished := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		defer close(cancelFinished)
		_ = conn.Close()
	})
	results, decodeErr := conn.PipelineBodies(pp.messageIDs, pp.destinations)
	for i, result := range results {
		pp.record(i, result)
	}
	if !stopCancel() {
		<-cancelFinished
	}
	return decodeErr
}

// record keeps one body's outcome, adopting a decoded memory-mode body at
// once so a failover attempt skips it.
func (pp *prefetchPipeline) record(i int, result nntp.DecodedBodyResult) {
	switch {
	case len(result.Body) > 0:
		pp.decoded[i] = result.Body
		pp.bodyErrors[i] = nil
		if result.Error != nil || pp.destinations[i].Writer != nil {
			return
		}
		if n, adoptErr := pp.writers[i].Adopt(result.Body); adoptErr == nil && n > 0 {
			pp.written[i] = n
			pp.destinations[i].Skip = true
			pp.decoded[i] = nil
		}
	case result.Bytes > 0:
		pp.written[i] = result.Bytes
		pp.bodyErrors[i] = nil
	case result.Error != nil:
		pp.bodyErrors[i] = result.Error
	}
}

// finish commits or discards every claimed segment and returns the first
// failure.
func (pp *prefetchPipeline) finish(err error) error {
	firstErr := err
	for i, claim := range pp.claims {
		claimErr := pp.settle(i, err)
		if firstErr == nil {
			firstErr = claimErr
		}
		pp.sf.finishPrefetchClaim(claim, claimErr)
	}
	return firstErr
}

// settle finalizes claim i's writer when it holds data, else discards it.
func (pp *prefetchPipeline) settle(i int, err error) error {
	if i >= len(pp.writers) || pp.writers[i] == nil {
		if err == nil {
			return ErrCacheClosed
		}
		return err
	}
	writer := pp.writers[i]
	n, writeErr := pp.accepted(i, err)
	if writeErr == nil && n == 0 {
		writeErr = errNoDecodedData()
	}
	if writeErr != nil {
		writer.Discard()
		return writeErr
	}
	writer.Finalize()
	return nil
}

// accepted returns the bytes claim i's writer holds, adopting a decoded body
// still pending, or the error explaining why there are none.
func (pp *prefetchPipeline) accepted(i int, err error) (int64, error) {
	switch {
	case i < len(pp.written) && pp.written[i] > 0:
		return pp.written[i], nil
	case i < len(pp.decoded) && len(pp.decoded[i]) > 0:
		return pp.writers[i].Adopt(pp.decoded[i])
	case i < len(pp.bodyErrors) && pp.bodyErrors[i] != nil:
		return 0, pp.bodyErrors[i]
	case err != nil:
		return 0, err
	}
	return 0, errNoDecodedData()
}

func (sf *SegmentFetcher) prefetchBatch(segIndices []int) {
	// Preserve the per-article timeout budget now that one task may stream
	// multiple ordered bodies.
	fetchCtx, cancel := context.WithTimeout(sf.ctx, scaledTimeout(sf.downloadTimeout(), len(segIndices)))
	defer cancel()

	var err error
	for attempt := range sf.maxAttempts() {
		if attempt > 0 {
			if err = sf.backoff(fetchCtx, attempt); err != nil {
				break
			}
		}
		err = sf.fetchPrefetchBatch(fetchCtx, segIndices)
		if !retryable(err, attempt) || fetchCtx.Err() != nil {
			break
		}
	}

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		sf.logger.Debug().Err(err).
			Int("first_segment", segIndices[0]).
			Int("segments", len(segIndices)).
			Msg("prefetch pipeline failed")
	}
}

// scaledTimeout multiplies timeout by n without overflowing.
func scaledTimeout(timeout time.Duration, n int) time.Duration {
	count := int64(max(n, 1))
	if int64(timeout) > math.MaxInt64/count {
		return math.MaxInt64
	}
	return time.Duration(int64(timeout) * count)
}

// defaultMaxAttempts is the retry budget when unconfigured.
const defaultMaxAttempts = 3

func (sf *SegmentFetcher) maxAttempts() int {
	if sf.config.MaxRetries < 1 {
		return defaultMaxAttempts
	}
	return sf.config.MaxRetries
}

// backoff waits before retry attempt, returning ctx's error if it ends first.
func (sf *SegmentFetcher) backoff(ctx context.Context, attempt int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sf.ctx.Done():
		return sf.ctx.Err()
	case <-time.After(sf.retryBackoff(attempt)):
		return nil
	}
}

// retryable reports whether err after attempt deserves another try: not on
// success or permanent errors, and — since the failover layer already
// retried per provider — at most one outer pass after all providers failed.
func retryable(err error, attempt int) bool {
	switch {
	case err == nil, nntp.IsArticleNotFoundError(err), nntp.IsYencDecodeError(err):
		return false
	case nntp.IsAllProvidersFailed(err):
		return attempt < 1
	}
	return true
}

// EnsureSegments fetches all missing segments through the shared scheduler.
func (sf *SegmentFetcher) EnsureSegments(ctx context.Context, startSeg, endSeg int) error {
	return sf.PrepareSegments(ctx, startSeg, endSeg)()
}

// PrepareSegments submits all required downloads before returning a waiter.
// Callers can enqueue speculative work after demand owns its queue position.
func (sf *SegmentFetcher) PrepareSegments(ctx context.Context, startSeg, endSeg int) func() error {
	var missing []int
	for i := startSeg; i <= endSeg; i++ {
		if sf.cache.GetState(i) != StateOnDisk {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return func() error { return nil }
	}

	waits := make([]<-chan error, len(missing))
	for j, segIdx := range missing {
		waits[j] = sf.schedule(ctx, priorityDemand, func() error {
			return sf.fetchWithRetryDirect(sf.ctx, segIdx, nntp.WorkloadStreamDemand)
		})
	}
	return func() error {
		for _, wait := range waits {
			if err := sf.waitScheduled(ctx, wait); err != nil {
				return err
			}
		}
		return nil
	}
}

// CancelPendingPrefetch invalidates queued work in O(bitmap words). Tasks
// already running finish and remain reusable; stale queued tasks become no-ops.
func (sf *SegmentFetcher) CancelPendingPrefetch() {
	sf.prefetchGen.Add(1)
	var cancelled int64
	for i := range sf.prefetchQueued {
		cancelled += int64(bits.OnesCount64(sf.prefetchQueued[i].Swap(0)))
	}
	if cancelled > 0 {
		sf.stats.PrefetchCancelled.Add(cancelled)
	}
}

func (sf *SegmentFetcher) fetchWithRetryDirect(ctx context.Context, segIdx int, workload nntp.Workload) error {
	var err error
	for attempt := range sf.maxAttempts() {
		if attempt > 0 {
			// Clear the failed state so the segment can be re-fetched, then
			// back off briefly before retrying. ResetFailed is a CAS: if a
			// concurrent reader fetched the segment meanwhile it stays OnDisk.
			sf.cache.ResetFailed(segIdx)
			if waitErr := sf.backoff(ctx, attempt); waitErr != nil {
				return waitErr
			}
		}
		err = sf.fetchDirect(ctx, segIdx, workload)
		// Don't retry success, permanent errors or cancellations.
		if !retryable(err, attempt) || ctx.Err() != nil || sf.ctx.Err() != nil {
			return err
		}
	}
	return err
}

// retryBackoff returns the delay before the given (1-indexed) retry attempt.
func (sf *SegmentFetcher) retryBackoff(attempt int) time.Duration {
	base := sf.config.RetryDelay
	if base <= 0 {
		base = time.Second
	}
	d := base << (attempt - 1)
	d = min(d, maxRetryBackoff)
	return d
}

// Close cancels this reader's work. Shared scheduler workers outlive it.
func (sf *SegmentFetcher) Close() {
	sf.taskMu.Lock()
	if sf.closing {
		sf.taskMu.Unlock()
		return
	}
	sf.closing = true
	sf.taskMu.Unlock()
	sf.cancel()
	if sf.ownedScheduler {
		sf.scheduler.Close()
	}
	sf.taskWg.Wait()
}

// errNoDecodedData treats an article whose body decodes to nothing as
// missing: it exists on the server but is empty or corrupt.
func errNoDecodedData() error {
	return &nntp.Error{
		Type:    nntp.ErrorTypeArticleNotFound,
		Message: "article produced no data after decoding",
	}
}

// bitsPerWord is the width of one prefetch dedup bitmap word.
const bitsPerWord = 64

// maxRetryBackoff caps the exponential retry delay.
const maxRetryBackoff = 5 * time.Second

// Error types.
var (
	ErrSegmentNotFound = &segmentError{msg: "segment not found"}
	ErrCacheClosed     = &segmentError{msg: "cache closed"}
)

type segmentError struct {
	msg string
}

func (e *segmentError) Error() string {
	return e.msg
}
