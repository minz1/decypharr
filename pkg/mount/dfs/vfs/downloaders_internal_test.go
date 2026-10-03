package vfs

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs/ranges"
)

const (
	testKiB = int64(1024)
	testMiB = 1024 * testKiB
)

func TestCurrentKickerInterval(t *testing.T) {
	t.Parallel()
	dls := &Downloaders{}

	if got := dls.currentKickerInterval(); got != kickerInterval {
		t.Fatalf("unexpected interval without waiters: got %s, want %s", got, kickerInterval)
	}

	dls.waiterCount.Store(1)
	if got := dls.currentKickerInterval(); got != activeWaiterKickerInterval {
		t.Fatalf("unexpected interval with waiters: got %s, want %s", got, activeWaiterKickerInterval)
	}
}

func getMaxOffset(dl *downloader) int64 {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.maxOffset
}

func TestEnsureDownloaderLocked_ExtendsMissByReadAhead(t *testing.T) {
	t.Parallel()
	const (
		reqPos    = 10 * testMiB
		reqSize   = 128 * testKiB
		readAhead = 16 * testMiB
	)

	item := newTestItem(t, 64*testMiB)

	dl := &downloader{
		start:     reqPos,
		offset:    reqPos,
		maxOffset: reqPos + reqSize,
	}

	dls := &Downloaders{
		item:          item,
		chunkSize:     4 * testMiB,
		readAheadSize: readAhead,
		dls:           []*downloader{dl},
	}

	req := ranges.Range{Pos: reqPos, Size: reqSize}
	if err := dls.ensureDownloaderLocked(req, false); err != nil {
		t.Fatalf("ensureDownloaderLocked returned error: %v", err)
	}

	want := req.End() + readAhead
	got := getMaxOffset(dl)
	if got != want {
		t.Fatalf("unexpected maxOffset: got %d, want %d", got, want)
	}
}

func TestEnsureDownloaderLocked_CachedRequestPrefetchesGap(t *testing.T) {
	t.Parallel()
	const (
		reqPos    = 0
		reqSize   = 128 * testKiB
		readAhead = 16 * testMiB
	)

	// request is cached, look-ahead has a gap after 1 MiB
	item := newTestItem(t, 64*testMiB, ranges.Range{Pos: 0, Size: 1 * testMiB})

	dl := &downloader{
		start:     512 * testKiB,
		offset:    2 * testMiB,
		maxOffset: 2 * testMiB,
	}

	dls := &Downloaders{
		item:          item,
		chunkSize:     4 * testMiB,
		readAheadSize: readAhead,
		dls:           []*downloader{dl},
	}

	req := ranges.Range{Pos: reqPos, Size: reqSize}
	if err := dls.ensureDownloaderLocked(req, false); err != nil {
		t.Fatalf("ensureDownloaderLocked returned error: %v", err)
	}

	want := req.End() + readAhead
	got := getMaxOffset(dl)
	if got != want {
		t.Fatalf("unexpected maxOffset: got %d, want %d", got, want)
	}
}

func TestEnsureDownloaderLocked_CachedWindowFullDoesNotExtend(t *testing.T) {
	t.Parallel()
	const (
		reqPos    = 0
		reqSize   = 128 * testKiB
		readAhead = 16 * testMiB
	)

	// request + look-ahead fully cached
	item := newTestItem(t, 64*testMiB, ranges.Range{Pos: 0, Size: 32 * testMiB})

	dl := &downloader{
		start:     0,
		offset:    2 * testMiB,
		maxOffset: 2 * testMiB,
	}

	dls := &Downloaders{
		item:          item,
		chunkSize:     4 * testMiB,
		readAheadSize: readAhead,
		dls:           []*downloader{dl},
	}

	req := ranges.Range{Pos: reqPos, Size: reqSize}
	if err := dls.ensureDownloaderLocked(req, false); err != nil {
		t.Fatalf("ensureDownloaderLocked returned error: %v", err)
	}

	want := 2 * testMiB
	got := getMaxOffset(dl)
	if got != want {
		t.Fatalf("unexpected maxOffset when window is full: got %d, want %d", got, want)
	}
}

func TestStopAllClearsWaiters(t *testing.T) {
	t.Parallel()
	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)

	dls := &Downloaders{
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
	}

	errCh := make(chan error, 1)
	dls.waiters = append(dls.waiters, waiter{
		r:       ranges.Range{Pos: 0, Size: 1},
		errChan: errCh,
	})
	dls.waiterCount.Store(1)

	dls.StopAll()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected waiter to receive stop error")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waiter was not unblocked by StopAll")
	}

	if got := dls.waiterCount.Load(); got != 0 {
		t.Fatalf("unexpected waiter count after StopAll: got %d, want 0", got)
	}
}

// TestDownloadWaitsOutStoppingThenFailsClosed verifies that Download does not
// create waiters/downloaders while a StopAll is in flight — it parks on
// stopCond instead — and that once the teardown resolves to closed, it
// returns an error without ever having created any work.
func TestDownloadWaitsOutStoppingThenFailsClosed(t *testing.T) {
	t.Parallel()
	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	dls := &Downloaders{
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
		stopping:  true,
		manager:   &persistedNZBBackend{},
		item: &CacheItem{
			info: ItemInfo{Size: 1 * testMiB},
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- dls.Download(context.Background(), ranges.Range{Pos: 0, Size: 1})
	}()

	// Download should be parked in the stopping-wait loop, not yet failed or
	// having created any work.
	select {
	case err := <-errCh:
		t.Fatalf("Download returned %v before StopAll resolved", err)
	case <-time.After(50 * time.Millisecond):
	}

	dls.mu.Lock()
	if got := len(dls.waiters); got != 0 {
		t.Fatalf("Download created waiters while stopping: got %d, want 0", got)
	}
	if got := len(dls.dls); got != 0 {
		t.Fatalf("Download created downloaders while stopping: got %d, want 0", got)
	}
	if dls.streamID != "" {
		t.Fatal("Download registered a stream while stopping")
	}
	// Resolve the in-flight stop to closed and wake the waiter, mirroring
	// StopAll()'s handoff to Close().
	dls.closed = true
	dls.stopCondLocked().Broadcast()
	dls.mu.Unlock()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected Download to fail once downloaders closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Download did not return after stop resolved to closed")
	}

	if got := len(dls.waiters); got != 0 {
		t.Fatalf("Download created waiters: got %d, want 0", got)
	}
	if got := len(dls.dls); got != 0 {
		t.Fatalf("Download created downloaders: got %d, want 0", got)
	}
}

func TestCacheItemReleaseStopsDownloadersOnZeroOpens(t *testing.T) {
	t.Parallel()
	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)

	dls := &Downloaders{
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
	}

	item := &CacheItem{}
	item.downloaders.Store(dls)
	item.opens.Store(1)

	item.Release()

	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected downloader context to be canceled when opens reaches zero")
	}

	if got := item.opens.Load(); got != 0 {
		t.Fatalf("unexpected opens after release: got %d, want 0", got)
	}
}

// Stall detection now lives in the manager stream session (see
// TestSessionStallWatchdogRecovers); the downloader no longer runs its own
// no-progress watchdog.

// An idle kicker returns right after checkIdleTimeout, but closes its done
// channel only in a deferred call. A read landing in that gap used to see the
// channel still open, assume a kicker was running, and start none — leaving
// parked waiters without their safety-net ticker for the whole session.
func TestIdleRestartStartsKickerBeforeOldOneClosesDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dls := &Downloaders{ctx: ctx, cancel: cancel, idle: true}
	exiting := make(chan struct{}) // old kicker: decided to exit, done not yet closed
	dls.kickerDone = exiting

	dls.mu.Lock()
	dls.restartKickerIfIdleLocked()
	fresh := dls.kickerDone
	idle := dls.idle
	dls.mu.Unlock()

	if fresh == exiting {
		t.Fatal("no fresh kicker started while the idle one was still exiting")
	}
	if idle {
		t.Fatal("session still marked idle after restart")
	}
	cancel()
	select {
	case <-fresh:
	case <-time.After(5 * time.Second):
		t.Fatal("fresh kicker did not exit on cancel")
	}
}

// The idle timeout tears a session down like StopAll: it holds the session
// in stopping until stopped downloaders have exited, so an error one records
// on its way out cannot land in the fresh session's error budget.
func TestIdleTimeoutWaitsForStoppedDownloaders(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dls := &Downloaders{item: &CacheItem{info: ItemInfo{Size: testMiB}}}
		dl := &downloader{quit: make(chan struct{}), cancel: func() {}}
		dl.wg.Add(1) // the downloader goroutine is still finishing a read
		dls.dls = []*downloader{dl}
		dls.lastActivity.Store(time.Now().Add(-2 * idleTimeout).UnixNano())

		result := make(chan bool, 1)
		go func() { result <- dls.checkIdleTimeout() }()
		synctest.Wait()

		dls.mu.Lock()
		stopping := dls.stopping
		// The exiting downloader records its interrupted read.
		dls.errorCount++
		dls.lastErr = errors.New("read interrupted")
		dls.mu.Unlock()
		if !stopping {
			t.Error("session was not held in stopping while a downloader was still running")
		}
		dl.wg.Done()

		if !<-result {
			t.Fatal("idle timeout did not end the session")
		}
		dls.mu.Lock()
		defer dls.mu.Unlock()
		if dls.errorCount != 0 || dls.lastErr != nil {
			t.Fatalf("next session starts with error count %d (%v)", dls.errorCount, dls.lastErr)
		}
		if dls.stopping || !dls.idle {
			t.Fatalf("stopping = %v, idle = %v after teardown", dls.stopping, dls.idle)
		}
	})
}
