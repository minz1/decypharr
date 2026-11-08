package usenet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/fs"
	"github.com/sirrobot01/decypharr/pkg/usenet/fs/reader"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

const (
	bufferSize = 256 * 1024 // 256KB buffer for streaming
	// defaultReadAhead applies when the configured read-ahead is invalid.
	defaultReadAhead = 16 << 20
	// idleSweepInterval is how often idle readers are checked for teardown.
	idleSweepInterval = 30 * time.Second
	// preCacheEdgeBytes is warmed at each end of a file (~3 segments).
	preCacheEdgeBytes = 2 << 20
)

func (u *Usenet) acquireStreamBuffer() *[]byte {
	bufPtr, ok := u.streamBuffers.Get().(*[]byte)
	if !ok || cap(*bufPtr) < bufferSize {
		buf := make([]byte, bufferSize)
		return &buf
	}
	*bufPtr = (*bufPtr)[:bufferSize]
	return bufPtr
}

func (u *Usenet) releaseStreamBuffer(buf *[]byte) {
	if buf == nil || cap(*buf) < bufferSize {
		return
	}
	u.streamBuffers.Put(buf)
}

type fsEntry struct {
	fs            *fs.FS
	volumes       []*types.Volume
	reader        fs.PrefetchableReaderAt // Shared reader with prefetch capability
	readerSize    int64                   // Size of the volume
	readerCleanup func(context.Context)   // Stops the reader, bounded by its ctx
	readerOnce    sync.Once               // Ensures reader is created exactly once
	readerErr     error                   // Error from reader creation (if any)
	retention     Retention
	lifecycleMu   sync.Mutex // serializes acquire/release/idle teardown
	refCount      atomic.Int32
	lastAccessed  atomic.Int64 // Unix timestamp
}

// fsEntryTombstone marks an entry claimed for teardown. Once refCount holds
// this value no new stream can acquire the entry (see acquire), which is what
// makes cleanup safe against a concurrent Stream that already Load()ed the
// entry from the map.
const fsEntryTombstone = int32(-1 << 30)

// cleanup stops the entry's reader and waits for it to release its memory,
// bounded by ctx. An entry whose reader was never created has nothing to stop.
// The reader fields are cleared under lifecycleMu, since finalize may clean an
// entry that a stream still holds and releases concurrently; the stop itself
// runs outside the lock.
func (fe *fsEntry) cleanup(ctx context.Context) {
	fe.lifecycleMu.Lock()
	stop := fe.readerCleanup
	fe.readerCleanup = nil
	fe.reader = nil
	fe.lifecycleMu.Unlock()
	if stop != nil {
		stop(ctx)
	}
}

// acquire takes a reference unless the entry has been claimed for teardown.
func (fe *fsEntry) acquire() bool {
	fe.lifecycleMu.Lock()
	defer fe.lifecycleMu.Unlock()
	n := fe.refCount.Load()
	if n < 0 {
		return false
	}
	fe.refCount.Store(n + 1)
	return true
}

// claimForCleanup atomically claims an idle (refCount == 0) entry for
// teardown, fencing out any future acquire.
func (fe *fsEntry) claimForCleanup() bool {
	fe.lifecycleMu.Lock()
	defer fe.lifecycleMu.Unlock()
	return fe.refCount.CompareAndSwap(0, fsEntryTombstone)
}

// release drops one handle reference. Delivery readers shed resident extents
// when the final downstream handle closes, while keeping metadata warm until
// the ordinary idle janitor removes the entry.
func (fe *fsEntry) release() {
	fe.lifecycleMu.Lock()
	defer fe.lifecycleMu.Unlock()
	if fe.refCount.Add(-1) != 0 || fe.retention != RetentionDelivery {
		return
	}
	if releaser, ok := fe.reader.(interface{ ReleaseIdleDelivery() }); ok {
		releaser.ReleaseIdleDelivery()
	}
}

// getOrCreateReader returns the shared reader, creating it lazily on first use.
// Uses [sync.Once] to ensure the reader is created exactly once even under concurrent access.
func (fe *fsEntry) getOrCreateReader() (fs.PrefetchableReaderAt, int64, error) {
	fe.readerOnce.Do(func() {
		var readerAt fs.PrefetchableReaderAt
		var size int64
		var cleanup func(context.Context)
		var err error

		// Single volume optimization - skip multi-volume overhead
		if len(fe.volumes) == 1 {
			readerAt, size, cleanup, err = fe.fs.CreateReaderAtForVolume(fe.volumes[0])
		} else {
			// Multi-volume case - need to create reader differently
			// For now, fall back to io.ReaderAt (no prefetch for multi-volume)
			var plainReaderAt io.ReaderAt
			var closeFiles func()
			plainReaderAt, size, closeFiles, err = fe.fs.CreateReaderAt()
			if err != nil {
				fe.readerErr = err
				return
			}
			// File.Close starts each volume reader's shutdown without waiting; the
			// pools' group waits for them.
			cleanup = func(context.Context) { closeFiles() }
			// Wrap in a no-op prefetchable reader
			readerAt = &noPrefetchReader{ReaderAt: plainReaderAt}
		}

		if err != nil {
			fe.readerErr = err
			return
		}

		fe.lifecycleMu.Lock()
		fe.reader = readerAt
		fe.readerSize = size
		fe.readerCleanup = cleanup
		fe.lifecycleMu.Unlock()
	})

	if fe.readerErr != nil {
		return nil, 0, fe.readerErr
	}
	// cleanup() nils the reader after the Once has fired; a caller racing a
	// shutdown-path cleanup must get an error, not a nil interface.
	fe.lifecycleMu.Lock()
	readerAt := fe.reader
	fe.lifecycleMu.Unlock()
	if readerAt == nil {
		return nil, 0, fmt.Errorf("reader has been closed")
	}
	return readerAt, fe.readerSize, nil
}

// noPrefetchReader wraps [io.ReaderAt] for cases where prefetch isn't available.
type noPrefetchReader struct {
	io.ReaderAt
}

func (n *noPrefetchReader) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	nr, err := n.ReaderAt.ReadAt(p, off)
	if ctxErr := ctx.Err(); ctxErr != nil && nr == 0 {
		return 0, ctxErr
	}
	return nr, err
}

func (n *noPrefetchReader) Prefetch(_ context.Context, _, _ int64) {
	// No-op for multi-volume readers
}

// OpenCursor returns a pass-through cursor: multi-volume readers have no
// shared prefetch/eviction state to isolate per consumer.
func (n *noPrefetchReader) OpenCursor() reader.ReadCursor {
	return passthroughCursor{r: n}
}

type passthroughCursor struct {
	r fs.PrefetchableReaderAt
}

func (c passthroughCursor) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return c.r.ReadAtContext(ctx, p, off)
}
func (c passthroughCursor) Prefetch(ctx context.Context, off, length int64) {
	c.r.Prefetch(ctx, off, length)
}
func (c passthroughCursor) Close() {}

type contextSectionReader struct {
	ctx   context.Context
	r     reader.ReadCursor
	base  int64
	limit int64
	off   int64
}

func newContextSectionReader(ctx context.Context, r reader.ReadCursor, off, length int64) *contextSectionReader {
	return &contextSectionReader{
		ctx:   ctx,
		r:     r,
		base:  off,
		limit: length,
	}
}

func (r *contextSectionReader) Read(p []byte) (int, error) {
	if r.off >= r.limit {
		return 0, io.EOF
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	remaining := r.limit - r.off
	if int64(len(p)) > remaining {
		p = p[:int(remaining)]
	}
	n, err := r.r.ReadAtContext(r.ctx, p, r.base+r.off)
	r.off += int64(n)
	if errors.Is(err, io.EOF) && r.off < r.limit {
		return n, io.ErrUnexpectedEOF
	}
	if err == nil && r.off >= r.limit {
		return n, io.EOF
	}
	return n, err
}

type Usenet struct {
	// g is the usenet service's group: the service's components and tasks run on it.
	g *task.Group
	// work is the group of the idle sweep, the legacy migration and entry
	// creation; it is the newest child, so it stops first.
	work                     *task.Group
	bufferPools              *reader.Pools
	nntp                     *nntp.Client
	analyzer                 *parser.NZBParser
	fetchScheduler           *reader.FetchScheduler
	logger                   zerolog.Logger
	config                   *config.Store
	streamSettings           config.Usenet // fixed at creation: buffers and pipelining
	metadataDir              string
	markers                  *os.Root    // metadataDir, for NZB marker files
	nzbStorage               *NZBStorage // File-based NZB metadata storage
	maxConnections           int         // Provider-wide streaming scheduler width
	processingMaxConnections int         // Connections allocated per file for parsing and NZB downloads
	prefetchSize             int64       // Streaming prefetch size in bytes
	failedFiles              *xsync.Map[string, error]
	// streamBuffers stores *[]byte so Put does not box the slice header.
	streamBuffers sync.Pool

	fs *xsync.Map[string, *fsEntry]
}

type Retention = reader.Retention

const (
	RetentionWindow   = reader.RetentionWindow
	RetentionRewind   = reader.RetentionRewind
	RetentionDelivery = reader.RetentionDelivery
)

// fsKey identifies a file independently of its retention tier.
func fsKey(nzoID, filename string) string {
	buf := make([]byte, len(nzoID)+2+len(filename))
	n := copy(buf, nzoID)
	buf[n] = ':'
	buf[n+1] = ':'
	copy(buf[n+2:], filename)
	return string(buf)
}

// configuredRetention is the stream retention the usenet settings ask for:
// rewind files when a disk buffer is configured, a memory window otherwise.
func configuredRetention(cfg config.Usenet) Retention {
	if cfg.UsesDiskBuffer() {
		return RetentionRewind
	}
	return RetentionWindow
}

// New creates the usenet service on g, its service group, for the
// configuration published by store. Availability sampling is read live;
// everything else is fixed at creation. The service's goroutines are tasks on
// g's children (nntp, scheduler, pools, work) and on g itself (finalize), so
// stopping g stops the service, in that order reversed: work first, then the
// readers and buffers, the scheduler, the nntp client, and finally finalize.
func New(g *task.Group, store *config.Store, logs *logger.Factory) (*Usenet, error) {
	if g == nil {
		return nil, errors.New("usenet: nil service group")
	}
	cfg := store.Get()
	usenetConfig := cfg.Usenet
	if len(usenetConfig.Providers) == 0 {
		return nil, fmt.Errorf("no usenet providers configured")
	}
	_logger := logs.New("usenet")

	metadataDir := filepath.Join(cfg.Dir(), "usenet", "nzbs")
	if err := os.MkdirAll(metadataDir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create metadata dir: %w", err)
	}
	markers, err := os.OpenRoot(metadataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open metadata dir: %w", err)
	}

	// Create file-based NZB storage
	nzbStorage, err := NewNZBStorage(filepath.Join(cfg.Dir(), "usenet", metaDirName), logs.New("nzb-storage"))
	if err != nil {
		_ = markers.Close()
		return nil, fmt.Errorf("failed to create NZB storage: %w", err)
	}

	// Disk rewind files are session-scoped; clear any left by an interrupted run.
	if clearErr := clearStaleStreamCache(strings.TrimSpace(usenetConfig.DiskPath)); clearErr != nil {
		_ = markers.Close()
		return nil, clearErr
	}

	maxConns, processingMaxConns := connectionLimits(usenetConfig)
	prefetchSize, err := config.ParseSize(usenetConfig.ReadAhead)
	if err != nil {
		prefetchSize = defaultReadAhead
	}

	parts, err := buildParts(g, cfg, maxConns, logs)
	if err != nil {
		_ = markers.Close()
		return nil, err
	}

	u := &Usenet{
		g:           g,
		work:        parts.work,
		bufferPools: parts.pools,
		nzbStorage:  nzbStorage,
		nntp:        parts.client,
		analyzer: parser.NewParser(
			parts.client,
			processingMaxConns,
			_logger.With().Str("component", "parser").Logger(),
		).WithFileFilter(func(name string, size int64) error {
			return store.Get().ValidateFileAllowed(name, size)
		}),
		config:                   store,
		streamSettings:           usenetConfig,
		markers:                  markers,
		fetchScheduler:           parts.scheduler,
		logger:                   _logger,
		metadataDir:              metadataDir,
		maxConnections:           maxConns,
		processingMaxConnections: processingMaxConns,
		prefetchSize:             prefetchSize,
		fs:                       xsync.NewMap[string, *fsEntry](),
		failedFiles:              xsync.NewMap[string, error](),
	}

	if startErr := u.startTasks(); startErr != nil {
		parts.stop()
		_ = markers.Close()
		return nil, startErr
	}
	return u, nil
}

// startTasks starts the service's own tasks: the idle sweep and the legacy
// migration on the work group, and finalize on the service group.
func (u *Usenet) startTasks() error {
	if err := u.work.Go("idle-sweep", u.cleanupIdleFS); err != nil {
		return fmt.Errorf("start idle sweep: %w", err)
	}
	// One-time (idempotent) upgrade of any legacy protobuf meta files to the v2
	// codec. It never blocks startup; atomic rewrites keep concurrent reads
	// safe, and a stop between files leaves the rest for the next run.
	if err := u.work.Go("migrate-legacy", u.migrateLegacy); err != nil {
		return fmt.Errorf("start legacy migration: %w", err)
	}
	if err := u.g.Go("finalize", u.finalize); err != nil {
		return fmt.Errorf("start finalize: %w", err)
	}
	return nil
}

// migrateLegacy is the work group's legacy-migration task.
func (u *Usenet) migrateLegacy(ctx context.Context) error {
	if _, err := u.nzbStorage.MigrateLegacy(ctx); err != nil && ctx.Err() == nil {
		u.nzbStorage.logger.Warn().Err(err).Msg("Legacy NZB meta migration failed")
	}
	return nil
}

// finalize is the service group's own task. It runs once every child group
// has stopped, so no stream, entry creation or reader is left: it removes the
// entries from the map, stops their readers (already done, so each wait ends at
// once) and closes the marker directory. It is the single owner of that
// teardown.
func (u *Usenet) finalize(ctx context.Context) error {
	<-ctx.Done()
	u.logger.Info().Msg("Closing Usenet")

	entries := make([]*fsEntry, 0, u.fs.Size())
	u.fs.Range(func(_ string, entry *fsEntry) bool {
		entries = append(entries, entry)
		return true
	})
	u.fs.Clear()
	var cleanup sync.WaitGroup
	for _, entry := range entries {
		cleanup.Go(func() { entry.cleanup(ctx) })
	}
	cleanup.Wait()

	if err := u.markers.Close(); err != nil {
		u.logger.Warn().Err(err).Msg("Failed to close the NZB marker directory")
	}
	u.logger.Info().Msg("Usenet closed")
	return nil
}

// parts are the components New builds, each on its own child of the service
// group.
type parts struct {
	made      []*task.Group // in creation order
	client    *nntp.Client
	scheduler *reader.FetchScheduler
	pools     *reader.Pools
	work      *task.Group
}

// child makes the named child of g and remembers it for stop.
func (p *parts) child(g *task.Group, name string) *task.Group {
	child := g.Child(name)
	p.made = append(p.made, child)
	return child
}

// stop stops the children made so far, newest first, waiting for each. It
// undoes a failed New, before any work has run on them.
func (p *parts) stop() {
	for _, child := range slices.Backward(p.made) {
		child.Close()
		<-child.Done()
	}
}

// buildParts creates the nntp, scheduler, pools and work children of g, in
// that order: stop runs the other way. On error the children made so far are
// stopped.
func buildParts(g *task.Group, cfg *config.Config, maxConns int, logs *logger.Factory) (*parts, error) {
	p := &parts{}
	client, err := nntp.NewClient(p.child(g, "nntp"), cfg, logs.New("nntp-client"))
	if err != nil {
		p.stop()
		return nil, err
	}
	p.client = client

	scheduler, err := reader.NewFetchScheduler(p.child(g, "scheduler"), maxConns)
	if err != nil {
		p.stop()
		return nil, fmt.Errorf("create fetch scheduler: %w", err)
	}
	p.scheduler = scheduler

	pools, err := reader.NewPools(p.child(g, "pools"), cfg.Usenet.BufferMemoryBytes())
	if err != nil {
		p.stop()
		return nil, fmt.Errorf("create buffer pools: %w", err)
	}
	p.pools = pools

	p.work = p.child(g, "work")
	return p, nil
}

// connectionLimits returns the streaming scheduler width and the per-file
// processing width for cfg, defaulting each when unset.
func connectionLimits(cfg config.Usenet) (int, int) {
	maxConns := cfg.MaxConnections
	if maxConns <= 0 {
		maxConns = 10
	}
	processingMaxConns := cfg.ProcessingMaxConnections
	if processingMaxConns <= 0 {
		processingMaxConns = maxConns
	}
	return maxConns, processingMaxConns
}

// clearStaleStreamCache removes rewind files ("cache-*") left in dir by an
// interrupted run. An empty dir means no disk cache is configured.
func clearStaleStreamCache(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create Usenet stream cache: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read Usenet stream cache: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "cache-") {
			continue
		}
		if removeErr := os.RemoveAll(filepath.Join(dir, entry.Name())); removeErr != nil {
			return fmt.Errorf("clear stale Usenet stream cache %q: %w", entry.Name(), removeErr)
		}
	}
	return nil
}

// createEntry builds a standalone fs entry for one file. prefetchSize sets the
// reader's read-ahead window; pass 0 for one-shot reads (head verification) so
// a single small read doesn't queue a full window of article downloads. It
// holds a lease on the work group for its whole body, so a stop waits for it
// and a stopped service refuses it with [task.ErrStopped].
func (u *Usenet) createEntry(file *storage.NZBFile, prefetchSize int64, retention Retention) (*fsEntry, error) {
	leave, err := u.work.Enter("entry")
	if err != nil {
		return nil, fmt.Errorf("create entry for %s: %w", file.Name, err)
	}
	defer leave()
	return u.buildEntry(file, prefetchSize, retention)
}

// buildEntry is createEntry's body; the caller holds the work lease.
func (u *Usenet) buildEntry(file *storage.NZBFile, prefetchSize int64, retention Retention) (*fsEntry, error) {
	volumes := GetFileVolumes(file)
	if len(volumes) == 0 {
		return nil, fmt.Errorf("no volumes available for file %s", file.Name)
	}

	usenetFS, err := fs.NewFS(
		u.bufferPools.Context(),
		u.nntp,
		u.maxConnections,
		prefetchSize,
		volumes,
		u.logger,
		fs.WithRetention(retention),
		fs.WithFetchScheduler(u.fetchScheduler),
		fs.WithPools(u.bufferPools),
		fs.WithBodyPipelineDepth(u.streamSettings.BodyPipelineDepth),
		fs.WithDiskPath(u.streamSettings.DiskPath),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create usenet FS: %w", err)
	}

	return &fsEntry{
		fs:        usenetFS,
		volumes:   volumes,
		retention: retention,
	}, nil
}

// entryKey is the cache key of one file at one retention tier.
func entryKey(nzoID, filename string, retention Retention) string {
	baseKey := fsKey(nzoID, filename)
	switch retention {
	case RetentionRewind:
		return baseKey + "::rewind"
	case RetentionDelivery:
		return baseKey + "::delivery"
	case RetentionWindow:
		// default key
	}
	return baseKey + "::window"
}

// getOrCreateEntry returns the shared fsEntry for one file with a reference
// taken. The caller releases that same entry, by pointer, with releaseUsed.
func (u *Usenet) getOrCreateEntry(
	ctx context.Context,
	nzoID, filename string,
	retention Retention,
) (*fsEntry, error) {
	key := entryKey(nzoID, filename, retention)

	// The lease covers both paths. Once the work group has stopped no entry
	// is handed out, not even one already in the map, since finalize is about
	// to stop its reader. On the slow path it spans the creation and the
	// store, so the work group's stop waits for both, and finalize (which
	// runs after it) finds every entry that was ever stored.
	leave, err := u.work.Enter("entry")
	if err != nil {
		return nil, fmt.Errorf("get entry for %s: %w", filename, err)
	}
	defer leave()

	// Fast path: entry already exists and isn't being torn down. acquire() (a
	// CAS, not a blind Add) is what closes the race against cleanupIdleFS:
	// once the janitor claims an idle entry no new reference can be taken, so
	// a stream can never end up on an entry whose reader is being closed.
	if entry, ok := u.fs.Load(key); ok && entry.acquire() {
		entry.lastAccessed.Store(time.Now().Unix())
		return entry, nil
	}

	file, err := u.getFile(nzoID, filename)
	if err != nil {
		return nil, err
	}

	// Pre-checks
	if preStreamChecksErr := u.preStreamChecks(file); preStreamChecksErr != nil {
		return nil, preStreamChecksErr
	}

	newEntry, err := u.buildEntry(file, u.prefetchSize, retention)
	if err != nil {
		return nil, err
	}
	return u.storeEntry(ctx, key, newEntry)
}

// storeEntry publishes newEntry under key, or takes the entry another creator
// published first. It returns the entry with a reference taken.
func (u *Usenet) storeEntry(ctx context.Context, key string, newEntry *fsEntry) (*fsEntry, error) {
	for {
		actual, loaded := u.fs.LoadOrStore(key, newEntry)
		if !loaded {
			// We won the race - use our new entry
			if !newEntry.acquire() {
				return nil, fmt.Errorf("new file-system entry was claimed during creation")
			}
			newEntry.lastAccessed.Store(time.Now().Unix())
			return newEntry, nil
		}
		// Another goroutine created the entry first - use theirs.
		// Our newEntry was never used (readers are lazy), GC reclaims it.
		if actual.acquire() {
			actual.lastAccessed.Store(time.Now().Unix())
			return actual, nil
		}
		// The mapped entry is claimed for teardown; the janitor removes it
		// from the map immediately after claiming, so retry until our entry
		// can be stored.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if u.work.Context().Err() != nil {
			return nil, task.ErrStopped
		}
		runtime.Gosched()
	}
}

// releaseUsed drops the reference acquire took and stamps the entry as just
// used. It acts on the entry the caller holds, never on whatever the map holds
// under the same key now.
func (fe *fsEntry) releaseUsed() {
	fe.lastAccessed.Store(time.Now().Unix())
	fe.release()
}

// entryCleanupBudget bounds the wait for a one-shot entry's reader to stop.
const entryCleanupBudget = 10 * time.Second

// cleanupOneShot stops the reader of an entry that never entered the map
// (head verification, decrypted download). The wait is bounded and does not
// end with the caller's ctx: the reader's memory must be released either way.
func (fe *fsEntry) cleanupOneShot(ctx context.Context) {
	cleanupCtx, cancel := task.Detached(ctx, entryCleanupBudget)
	defer cancel()
	fe.cleanup(cleanupCtx)
}

// cleanupIdleFS removes sessions with refCount=0 that haven't been used recently.
// It is the work group's idle-sweep task and ends with ctx.
func (u *Usenet) cleanupIdleFS(ctx context.Context) error {
	// Keep a warm reader through short pauses, then tear it down. Usenet segment
	// buffering is only for active latency hiding; stale buffers should disappear
	// quickly instead of behaving like a VFS cache.
	const idleThreshold = int64(120) // 2 minutes idle
	ticker := time.NewTicker(idleSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		now := time.Now().Unix()

		u.fs.Range(func(key string, entry *fsEntry) bool {
			if entry.refCount.Load() == 0 {
				lastUsed := entry.lastAccessed.Load()
				if now-lastUsed > idleThreshold {
					// Claim before touching anything: the CAS fences out a
					// concurrent Stream that already Load()ed this entry from
					// the map (it will fail acquire() and create a fresh
					// entry). Delete from the map before the (potentially
					// slow) cleanup so waiting creators aren't stalled.
					if entry.claimForCleanup() {
						u.fs.Delete(key)
						entry.cleanup(ctx)
					}
				}
			}
			return true
		})
	}
}

// Parse processes an NZB for download/streaming (quick parse, defers archive extraction).
func (u *Usenet) Parse(
	ctx context.Context,
	name string,
	content []byte,
	category string,
) (*storage.NZB, map[string]*parser.FileGroup, error) {
	return u.ParseWithID(ctx, "", name, content, category)
}

// ParseWithID parses an NZB using a caller-provided ID. Supplying the ID lets
// the manager expose a queued entry before the active-download worker starts.
func (u *Usenet) ParseWithID(
	ctx context.Context,
	id, name string,
	content []byte,
	category string,
) (*storage.NZB, map[string]*parser.FileGroup, error) {
	if len(content) == 0 {
		return nil, nil, fmt.Errorf("NZB content is empty")
	}

	// Validate NZB content
	if err := validateNZB(content); err != nil {
		return nil, nil, fmt.Errorf("invalid NZB content: %w", err)
	}

	// Quick parse: defer archive extraction for async processing.
	// Groups survive a stat failure so the caller can identify the
	// dead post.
	nzb, groups, err := u.analyzer.Parse(ctx, name, content)
	if err != nil {
		return nil, groups, err
	}
	if id != "" {
		nzb.ID = id
	}

	nzb.Category = category
	nzb.Status = NZBStatusParsing
	// Save NZB file to disk
	nzbPath, err := u.saveNZBFile(nzb.ID, content)
	if err != nil {
		return nil, nil, err
	}
	nzb.Path = nzbPath

	// Mark as processing
	if markAsProcessingErr := u.markAsProcessing(nzb); markAsProcessingErr != nil {
		// Don't leave the source file orphaned; an un-marked .nzb would be
		// re-claimed by the refresh watcher on every scan.
		_ = os.Remove(nzbPath)
		return nil, nil, fmt.Errorf("failed to mark NZB as processing: %w", markAsProcessingErr)
	}

	if addNZBErr := u.nzbStorage.AddNZB(nzb); addNZBErr != nil {
		u.removeProcessingMarker(nzbPath)
		_ = os.Remove(nzbPath)
		return nil, nil, fmt.Errorf("failed to save NZB to storage: %w", addNZBErr)
	}

	u.logger.Info().
		Str("nzb_id", nzb.ID).
		Str("name", nzb.Name).
		Int("groups", len(groups)).
		Msg("Successfully parsed NZB file")
	return nzb, groups, nil
}

// Process processes archive files in an NZB (full parse).
func (u *Usenet) Process(
	ctx context.Context,
	nzb *storage.NZB,
	groups map[string]*parser.FileGroup,
) (*storage.NZB, error) {
	u.logger.Info().
		Str("nzb_id", nzb.ID).
		Str("name", nzb.Name).
		Msg("Processing archive files in NZB")

	// Process the groups (archives)
	updatedNZB, err := u.analyzer.Process(ctx, nzb, groups)
	if err != nil {
		// Mark as failed
		u.failUnlessCancelled(ctx, nzb, err)
		return nzb, fmt.Errorf("failed to process NZB archives: %w", err)
	}
	if finishErr := u.finish(ctx, updatedNZB); finishErr != nil {
		return updatedNZB, finishErr
	}

	u.logger.Info().
		Str("nzb_id", updatedNZB.ID).
		Str("name", updatedNZB.Name).
		Int("files", len(updatedNZB.Files)).
		Msg("Successfully processed NZB archives (full parse)")
	return updatedNZB, nil
}

// finish runs the availability and content gates on a processed NZB and
// records the outcome: completed, or failed with the reason.
func (u *Usenet) finish(ctx context.Context, nzb *storage.NZB) error {
	// Post-parse availability gate: probe a sample of each content file's
	// segments before declaring the NZB complete. Segments can go missing
	// between the original parse and now; without this gate they slip through
	// to Sonarr/Radarr and only surface later as failed ffprobes. Connection
	// errors are non-fatal here (CheckFileAvailability returns nil for those),
	// so a provider hiccup won't wrongly fail an import — only a definitively
	// missing segment (gone on every provider) fails the NZB.
	if checkNZBAvailabilityErr := u.checkNZBAvailability(ctx, nzb); checkNZBAvailabilityErr != nil {
		u.failUnlessCancelled(ctx, nzb, checkNZBAvailabilityErr)
		return fmt.Errorf("availability check failed: %w", checkNZBAvailabilityErr)
	}

	// Content gate: availability only proves the articles exist; it says
	// nothing about whether the assembled stream is the file it claims to be.
	// A wrong RAR volume order or a misordered raw post passes every STAT
	// probe yet serves garbage from byte 0 (players see no codec and a bogus
	// size/bitrate runtime). Read each media file's head through the real
	// serving path and require a container signature, so a scrambled assembly
	// fails here — and the arr grabs a replacement — instead of reaching the
	// library as an unplayable file.
	if verifyNZBContentErr := u.verifyNZBContent(ctx, nzb); verifyNZBContentErr != nil {
		u.failUnlessCancelled(ctx, nzb, verifyNZBContentErr)
		return fmt.Errorf("content verification failed: %w", verifyNZBContentErr)
	}

	// Both gates pass over a cancelled context without checking anything,
	// so an interrupted run proves nothing about the content: never complete
	// it unverified. A shutdown leaves it parsing; a timeout fails it.
	if ctxErr := ctx.Err(); ctxErr != nil {
		u.failUnlessCancelled(ctx, nzb, ctxErr)
		return fmt.Errorf("processing interrupted: %w", ctxErr)
	}

	if markAsCompletedErr := u.markAsCompleted(nzb); markAsCompletedErr != nil {
		return fmt.Errorf("failed to mark NZB as completed: %w", markAsCompletedErr)
	}
	return nil
}

// checkAvailability samples each content file's segments (via the same
// repair-bank-gated BatchStat path as CheckFile) and returns an error if any
// file is definitively unavailable — i.e. a sampled segment is missing on
// every provider. Ignored, deleted, and
// segment-less entries are skipped so the gate fails only on genuinely missing
// playable content. Connection-only failures are treated as non-fatal by
// CheckFileAvailability, so they do not fail the NZB. It returns on the first
// definitively-missing file (fail fast).
func (u *Usenet) checkNZBAvailability(ctx context.Context, nzb *storage.NZB) error {
	samplePercent := u.config.Get().Usenet.ImportAvailabilitySamplePercent
	for i := range nzb.Files {
		file := &nzb.Files[i]
		if file.IsDeleted || len(file.Segments) == 0 {
			continue
		}
		if file.FileType == storage.NZBFileTypeIgnore {
			continue
		}
		select {
		case <-ctx.Done():
			// Cancelled/timed out: not a content failure — don't fail the NZB.
			return nil
		default:
		}
		if err := u.CheckFileAvailability(ctx, file, samplePercent); err != nil {
			u.logger.Warn().
				Err(err).
				Str("nzb_id", nzb.ID).
				Str("file", file.Name).
				Msg("Post-parse availability check failed; marking NZB unavailable")
			return fmt.Errorf("file %q unavailable: %w", file.Name, err)
		}
	}
	return nil
}

// CheckFile probes the availability of a single NZB file. Connection use is
// gated by the NNTP client's repair bank so concurrent probes don't starve
// streaming traffic.
func (u *Usenet) CheckFile(ctx context.Context, nzoID, filename string) error {
	messageIDs, err := u.fileMessageIDs(nzoID, filename, u.config.Get().Usenet.AvailabilitySamplePercent)
	if err != nil {
		return err
	}
	return u.checkAvailability(ctx, filename, messageIDs)
}

// CheckWholeFile STATs every article of one file, including ones seen at import: a stream just failed on one of them.
func (u *Usenet) CheckWholeFile(ctx context.Context, nzoID, filename string) error {
	messageIDs, err := u.fileMessageIDs(nzoID, filename, wholeFilePercent)
	if err != nil {
		return err
	}
	return u.statAvailability(ctx, filename, messageIDs)
}

const wholeFilePercent = 100

func (u *Usenet) fileMessageIDs(nzoID, filename string, samplePercent int) ([]string, error) {
	// Repair/availability probes only need a sample of one file's message ids.
	// Decode just those (no numeric columns, no NZBSegment structs, no other
	// files) so a full sweep doesn't hold whole segment maps in memory.
	messageIDs, err := u.nzbStorage.SampleFileMessageIDs(nzoID, filename, samplePercent)
	if err != nil {
		return nil, fmt.Errorf("failed to sample file segments: %w", err)
	}
	if len(messageIDs) == 0 {
		return nil, fmt.Errorf("file has no Segments: %s: %w", filename, customerror.ErrUsenetManifestInvalid)
	}
	return messageIDs, nil
}

func (u *Usenet) CheckFileAvailability(ctx context.Context, file *storage.NZBFile, samplePercent int) error {
	return u.checkAvailability(ctx, file.Name, u.sampleSegments(file.Segments, samplePercent))
}

// checkAvailability batch-STATs the given sampled message ids. The NNTP client
// gates each worker through its internal repair bank so concurrent availability
// checks don't starve streaming connections.
func (u *Usenet) checkAvailability(ctx context.Context, fileName string, messageIDs []string) error {
	return u.statAvailability(ctx, fileName, u.analyzer.FilterUnobserved(messageIDs))
}

func (u *Usenet) statAvailability(ctx context.Context, fileName string, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return nil
	}

	result, err := u.nntp.BatchStat(ctx, messageIDs)
	if err != nil {
		// Connection/system error - log and continue (don't fail availability check)
		u.logger.Warn().
			Err(err).
			Str("file", fileName).
			Msg("Non-fatal error during availability check, ignoring")
		return nil
	}

	// Check if all sampled segments are available.
	// Distinguish genuine article-not-found from connection errors:
	//   TotalCount = FoundCount + notFoundCount + ErrorCount
	// Only treat a file as unavailable when segments are definitively missing
	// (notFoundCount > 0). Connection errors mean we couldn't check — treat
	// those the same as the top-level error path above (non-fatal, skip check).
	if !result.AllAvailable() {
		notFoundCount := result.TotalCount - result.FoundCount - result.ErrorCount
		if result.ErrorCount > 0 && notFoundCount == 0 {
			// All failures were connection errors, not missing articles.
			return nil
		}
		// At least some segments are definitively missing.
		u.logger.Warn().
			Str("file", fileName).
			Int("sampled_segments", len(messageIDs)).
			Int("available_segments", result.FoundCount).
			Int("missing_segments", notFoundCount).
			Int("error_count", result.ErrorCount).
			Msg("File is unavailable - one or more segments are missing")
		return customerror.ErrUsenetSegmentMissing
	}

	return nil
}

// sampleSegments returns a sample of segment message IDs based on the given
// percentage. Always includes first and last segments, then uniformly samples
// from the middle (see sampleIndices).
func (u *Usenet) sampleSegments(segments []storage.NZBSegment, percent int) []string {
	idx := sampleIndices(len(segments), percent)
	if len(idx) == 0 {
		return nil
	}
	out := make([]string, len(idx))
	for i, j := range idx {
		out[i] = segments[j].MessageID
	}
	return out
}

func (u *Usenet) getFile(nzoID, filename string) (*storage.NZBFile, error) {
	file, err := u.nzbStorage.GetNZBFile(nzoID, filename)
	if errors.Is(err, errFileNotFound) {
		return nil, fmt.Errorf("file %s not found in NZB %s: %w", filename, nzoID, customerror.ErrUsenetManifestInvalid)
	}
	if err != nil {
		return nil, fmt.Errorf("metadata load failed: %w", err)
	}
	if file.NzbID == "" {
		file.NzbID = nzoID
	}
	return file, nil
}

func (u *Usenet) preStreamChecks(file *storage.NZBFile) error {
	// Check if we have Segments
	if len(file.Segments) == 0 {
		return fmt.Errorf("file has no Segments: %s", file.Name)
	}

	// Check if file was marked as failed previously
	if cause, ok := u.failedFiles.Load(fsKey(file.NzbID, file.Name)); ok {
		if nntp.IsArticleNotFoundError(cause) || customerror.IsArticleNotFoundError(cause) {
			return customerror.NewArticleNotFoundError(cause)
		}
		return customerror.NewSilentError(cause).Permanent()
	}

	return nil
}

// FileHandle is a pull-based handle over one usenet file, backed by the
// entry's shared reader through a private cursor. It holds the entry
// refcount for its lifetime (blocking idle teardown) and releases it on
// Close.
type FileHandle struct {
	u          *Usenet
	failureKey string
	entry      *fsEntry
	cursor     reader.ReadCursor
	size       int64
	closed     atomic.Bool
}

// OpenFile opens a pull-based handle for one file of an NZB. The returned
// handle must be Closed; it pins the underlying fs entry (and its reader)
// while open.
func (u *Usenet) OpenFile(ctx context.Context, nzoID, filename string) (*FileHandle, error) {
	return u.OpenFileWithRetention(ctx, nzoID, filename, configuredRetention(u.streamSettings))
}

func (u *Usenet) OpenFileWithRetention(
	ctx context.Context,
	nzoID, filename string,
	retention Retention,
) (*FileHandle, error) {
	entry, err := u.getOrCreateEntry(ctx, nzoID, filename, retention)
	if err != nil {
		return nil, fmt.Errorf("failed to get or create file system: %w", err)
	}
	readerAt, size, err := entry.getOrCreateReader()
	if err != nil {
		entry.releaseUsed()
		return nil, fmt.Errorf("failed to get reader: %w", err)
	}
	return &FileHandle{
		u:          u,
		failureKey: fsKey(nzoID, filename),
		entry:      entry,
		cursor:     readerAt.OpenCursor(),
		size:       size,
	}, nil
}

// Size returns the underlying volume size in bytes.
func (h *FileHandle) Size() int64 { return h.size }

// ReadAtContext reads at off through the handle's cursor. Article-not-found
// is surfaced as a permanent error and remembered, matching Stream.
func (h *FileHandle) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if h.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	h.entry.lastAccessed.Store(time.Now().Unix())
	n, err := h.cursor.ReadAtContext(ctx, p, off)
	if err != nil && ctx != nil && ctx.Err() != nil {
		return n, ctx.Err()
	}
	if err != nil && nntp.IsArticleNotFoundError(err) {
		h.u.failedFiles.Store(h.failureKey, err)
		return n, customerror.NewArticleNotFoundError(err)
	}
	return n, err
}

// Prefetch queues a bounded read-ahead window from off without blocking.
func (h *FileHandle) Prefetch(ctx context.Context, off, length int64) {
	if h.closed.Load() {
		return
	}
	if h.u.prefetchSize > 0 && length > h.u.prefetchSize {
		length = h.u.prefetchSize
	}
	h.cursor.Prefetch(ctx, off, length)
}

// ReleaseCachedRange acknowledges that a downstream persistent cache has
// accepted an independent copy. It remains valid during the narrow interval
// between a body closing and its session publishing the final write.
func (h *FileHandle) ReleaseCachedRange(off, length int64) {
	if releaser, ok := h.cursor.(interface{ ReleaseCachedRange(int64, int64) }); ok {
		releaser.ReleaseCachedRange(off, length)
	}
}

// Close releases the cursor and the entry reference. Idempotent.
func (h *FileHandle) Close() error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}
	h.cursor.Close()
	h.entry.releaseUsed()
	return nil
}

// Stream streams a file using the new streaming system with caching and worker limiting.
func (u *Usenet) Stream(ctx context.Context, nzoID, filename string, start, end int64, writer io.Writer) error {
	if start < 0 {
		start = 0
	}
	if end < start {
		return fmt.Errorf("invalid byte range %d-%d", start, end)
	}

	ufsEntry, err := u.getOrCreateEntry(ctx, nzoID, filename, configuredRetention(u.streamSettings))
	if err != nil {
		return fmt.Errorf("failed to get or create file system: %w", err)
	}
	defer ufsEntry.releaseUsed()

	// Use start/end directly - file segments are already positioned correctly
	rangeStart := start
	rangeEnd := end

	// Validate range against volume size
	if rangeEnd >= ufsEntry.volumes[0].Size {
		rangeEnd = ufsEntry.volumes[0].Size - 1
	}

	if rangeEnd < rangeStart {
		return fmt.Errorf("invalid resolved byte range %d-%d", rangeStart, rangeEnd)
	}

	// get shared reader from entry (created once, reused by all streams)
	readerAt, _, err := ufsEntry.getOrCreateReader()
	if err != nil {
		return fmt.Errorf("failed to get reader: %w", err)
	}

	length := rangeEnd - rangeStart + 1

	// Check context before starting
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Prefetch only a bounded read-ahead window from the requested start,
	// NOT the entire range. Queuing a whole multi-GB file would flood the
	// fixed-depth prefetch channel with head segments and starve reads that
	// land elsewhere (e.g. ffprobe seeking to the moov atom at EOF). The
	// per-read sliding window in readAtPlain advances this as playback
	// progresses; PreCache separately warms the head and tail.
	prefetchLen := length
	if u.prefetchSize > 0 && prefetchLen > u.prefetchSize {
		prefetchLen = u.prefetchSize
	}

	// Each Stream call is one consumer with its own cursor.
	cursor := readerAt.OpenCursor()
	defer cursor.Close()
	cursor.Prefetch(ctx, rangeStart, prefetchLen)

	section := newContextSectionReader(ctx, cursor, rangeStart, length)
	bufPtr := u.acquireStreamBuffer()
	defer u.releaseStreamBuffer(bufPtr)

	// Use a safe copy loop that checks context and validates read counts
	err = safeCopyBuffer(ctx, writer, section, *bufPtr)

	// Handle context cancellation explicitly
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}

	// Mark file as failed if article not found (permanent error)
	if err != nil && nntp.IsArticleNotFoundError(err) {
		u.failedFiles.Store(fsKey(nzoID, filename), err)
		// Wrap error to mark as permanent
		return customerror.NewArticleNotFoundError(err)
	}

	return err
}

// safeCopyBuffer copies from src to dst using buf, with context checking and
// validation of read counts to prevent panics from corrupted readers during shutdown.
func safeCopyBuffer(ctx context.Context, dst io.Writer, src io.Reader, buf []byte) error {
	if len(buf) == 0 {
		buf = make([]byte, bufferSize)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nr, er := src.Read(buf)
		// A count outside the buffer would panic the slice below; corrupted
		// readers during shutdown have produced them.
		if nr < 0 || nr > len(buf) {
			return fmt.Errorf("reader returned invalid count %d (buffer size %d)", nr, len(buf))
		}
		if nr > 0 {
			if ew := writeChunk(dst, buf[:nr]); ew != nil {
				return ew
			}
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				return nil
			}
			return er
		}
	}
}

// writeChunk writes p and validates the count the writer reports.
func writeChunk(dst io.Writer, p []byte) error {
	nw, err := dst.Write(p)
	switch {
	case err != nil:
		return err
	case nw < 0 || nw > len(p):
		return fmt.Errorf("invalid write count: %d", nw)
	case nw != len(p):
		return io.ErrShortWrite
	}
	return nil
}

// Touch validates that the first segment of a file is available via NNTP STAT.
func (u *Usenet) Touch(ctx context.Context, nzoID, filename string) error {
	file, err := u.getFile(nzoID, filename)
	if err != nil {
		return fmt.Errorf("failed to get file: %w", err)
	}

	if preStreamChecksErr := u.preStreamChecks(file); preStreamChecksErr != nil {
		return preStreamChecksErr
	}

	// Check if we have Segments
	if len(file.Segments) == 0 {
		return fmt.Errorf("file has no Segments: %s", filename)
	}

	// get first segment
	firstSeg := file.Segments[0]
	// Run STAT command to check if article exists
	_, _, err = u.nntp.Stat(ctx, nntp.WorkloadStreamDemand, firstSeg.MessageID)
	if err != nil {
		return fmt.Errorf("segment not available: %w", err)
	}
	return nil
}

// PreCache creates a file system entry and pre-fetches head and tail segments.
// This warms up the cache to reduce latency for subsequent reads (e.g. ffprobe).
// Uses the shared entry/reader so the cache is available for Stream calls.
func (u *Usenet) PreCache(ctx context.Context, nzoID, filename string) error {
	// Use shared entry (same as Stream)
	entry, err := u.getOrCreateEntry(ctx, nzoID, filename, configuredRetention(u.streamSettings))
	if err != nil {
		return fmt.Errorf("failed to get or create entry: %w", err)
	}
	defer entry.releaseUsed()

	if len(entry.volumes) == 0 {
		return fmt.Errorf("no volumes available for file %s", filename)
	}

	fileSize := entry.volumes[0].Size

	// Calculate how much to read for head and tail
	headSize := int64(preCacheEdgeBytes)
	tailSize := int64(preCacheEdgeBytes)

	if headSize > fileSize {
		headSize = fileSize
	}

	// get shared reader from entry
	readerAt, _, err := entry.getOrCreateReader()
	if err != nil {
		return fmt.Errorf("failed to get reader: %w", err)
	}

	// Pre-fetch head segments using Prefetch (non-blocking segment download)
	readerAt.Prefetch(ctx, 0, headSize)

	// Pre-fetch tail segments (if file is large enough)
	if fileSize > headSize+tailSize {
		tailOffset := fileSize - tailSize
		readerAt.Prefetch(ctx, tailOffset, tailSize)
	}

	return nil
}

// Stats returns nntp statistics.
func (u *Usenet) Stats() map[string]any {
	stats := u.nntp.Stats()
	stats["readers"] = u.fs.Size()
	stats["nzb_storage"] = u.nzbStorage.Stats()
	stats["analyzer"] = u.analyzer.Metrics()
	stats["buffers"] = u.bufferPools.Stats()
	return stats
}

// GetNZB returns NZB metadata by ID.
func (u *Usenet) GetNZB(id string) (*storage.NZB, error) {
	return u.nzbStorage.GetNZB(id)
}

// GetNZBHeader returns NZB metadata without its segment map. Use this when only
// scalar fields or the file list are needed (status, path, sizes); it avoids
// decoding/allocating the multi-megabyte segment data.
func (u *Usenet) GetNZBHeader(id string) (*storage.NZB, error) {
	return u.nzbStorage.GetNZBHeader(id)
}

// ForEachNZB iterates over all NZBs.
func (u *Usenet) ForEachNZB(fn func(*storage.NZB) error) error {
	return u.nzbStorage.ForEachNZB(fn)
}

// NZBStorage returns the underlying NZB storage.
func (u *Usenet) NZBStorage() *NZBStorage {
	return u.nzbStorage
}

// SpeedTest runs a speed test for a specific NNTP provider, identified by
// its canonical ID (host:port/username); a bare host works when unambiguous.
// It finds a segment from a processed NZB to download for real speed measurement.
func (u *Usenet) SpeedTest(ctx context.Context, providerID string) nntp.SpeedTestResult {
	// Try to find a segment from any processed NZB for the speed test
	messageID := u.findTestSegment()
	return u.nntp.SpeedTest(ctx, providerID, messageID)
}

// findTestSegment looks for a segment from any processed NZB to use for speed testing.
func (u *Usenet) findTestSegment() string {
	var messageID string

	// Iterate through NZBs to find a usable segment
	_ = u.nzbStorage.ForEachNZB(func(nzb *storage.NZB) error {
		for _, file := range nzb.Files {
			if file.IsDeleted || len(file.Segments) == 0 {
				continue
			}
			// Use the first segment we find
			messageID = file.Segments[0].MessageID
			// Return an error to stop iteration (not a real error)
			return fmt.Errorf("found")
		}
		return nil
	})

	return messageID
}

// GetSpeedTestResults returns all stored speed test results.
func (u *Usenet) GetSpeedTestResults() map[string]nntp.SpeedTestResult {
	return u.nntp.GetSpeedTestResults()
}

func (u *Usenet) saveNZBFile(id string, content []byte) (string, error) {
	// Store the raw source keyed by the bounded NZB ID rather than the
	// (untrusted, arbitrarily long) display name. ext4 caps a path component at
	// 255 bytes; a long release name plus a ".processing"/".importing"/".queued"
	// marker suffix blew past that limit, which failed the rename, wedged the
	// refresh watcher, and left truncated fragment files behind. The UUID keeps
	// every derived name comfortably under the cap.
	path := filepath.Join(u.metadataDir, id+".nzb")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", fmt.Errorf("failed to save NZB file to disk: %w", err)
	}
	return path, nil
}

// StageNZB persists a queued NZB before an active-download worker starts.
func (u *Usenet) StageNZB(id string, content []byte) (string, error) {
	if id == "" {
		return "", fmt.Errorf("NZB ID is required")
	}
	// Keep the staged file off the .nzb extension so the metadata-directory
	// watcher does not treat a pending active-download job as an unmanaged import.
	path := filepath.Join(u.metadataDir, id+".queued")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", fmt.Errorf("failed to stage NZB file: %w", err)
	}
	return path, nil
}

// RemoveStagedNZB removes a queued source file after it has been parsed.
func (u *Usenet) RemoveStagedNZB(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// markerFileMode is the mode of NZB marker files: private to the service.
const markerFileMode os.FileMode = 0o600

// processingMarker names the ".processing" marker of an NZB source file,
// relative to the metadata directory. ok is false when the NZB has no source
// file there; markers never resolve anywhere else (an empty path would
// otherwise name a file in the working directory).
func (u *Usenet) processingMarker(nzbPath string) (string, bool) {
	if nzbPath == "" {
		return "", false
	}
	rel, err := filepath.Rel(u.metadataDir, nzbPath)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	return rel + ".processing", true
}

// removeProcessingMarker deletes the ".processing" marker of an NZB source
// file, if it has one.
func (u *Usenet) removeProcessingMarker(nzbPath string) {
	if name, ok := u.processingMarker(nzbPath); ok {
		_ = u.markers.Remove(name)
	}
}

func (u *Usenet) markAsProcessing(nzb *storage.NZB) error {
	// Mark as processing by creating a marker file with the NZB ID
	name, ok := u.processingMarker(nzb.Path)
	if !ok {
		return fmt.Errorf("NZB source %q is outside the metadata directory", nzb.Path)
	}
	if err := u.markers.WriteFile(name, []byte(nzb.ID), markerFileMode); err != nil {
		return fmt.Errorf("failed to create processing marker: %w", err)
	}
	return nil
}

// markAsCompleted records the NZB as completed and only then releases its
// source file and marker. A failed write leaves the persisted "parsing" record
// and the source it points to intact, so the NZB can still be re-driven or
// failed instead of being stranded on a deleted file. A crash between the write
// and the removals leaves an orphan .nzb and .processing marker behind: a leak,
// not data loss.
func (u *Usenet) markAsCompleted(nzb *storage.NZB) error {
	source, status := nzb.Path, nzb.Status
	nzb.Status = NZBStatusCompleted
	nzb.Path = ""
	if err := u.nzbStorage.AddNZB(nzb); err != nil {
		nzb.Status, nzb.Path = status, source
		return fmt.Errorf("failed to save NZB to storage: %w", err)
	}

	if source != "" {
		if err := os.Remove(source); err != nil && !os.IsNotExist(err) {
			u.logger.Warn().Err(err).Str("path", source).Msg("Failed to delete NZB source file after completion")
		}
		u.removeProcessingMarker(source)
	}
	return nil
}

// failUnlessCancelled records err as nzb's failure, unless ctx was cancelled.
// A cancelled run is a shutdown, which proves nothing about the NZB: it stays
// parsing so the restore re-drives it. A deadline is the NZB's own failure and
// is recorded like any other. A failed write is not reported: the NZB then
// stays parsing and the caller's error still reaches the manager.
func (u *Usenet) failUnlessCancelled(ctx context.Context, nzb *storage.NZB, err error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	_ = u.markAsFailed(nzb, err)
}

func (u *Usenet) markAsFailed(nzb *storage.NZB, err error) error {
	// Mark as failed in storage
	nzb.Status = NZBStatusFailed
	nzb.FailMessage = err.Error()
	if addNZBErr := u.nzbStorage.AddNZB(nzb); addNZBErr != nil {
		return fmt.Errorf("failed to mark NZB as failed in storage: %w", addNZBErr)
	}

	// Remove the processing marker and the nzb file itself, as it's
	// considered failed. Without a path there is nothing to remove.
	if nzb.Path != "" {
		u.removeProcessingMarker(nzb.Path)
		if removeErr := os.Remove(nzb.Path); removeErr != nil && !os.IsNotExist(removeErr) {
			u.logger.Warn().
				Err(removeErr).
				Str("path", nzb.Path).
				Msg("Failed to delete NZB file from disk after failure")
		}
	}
	return nil
}

// Fail marks an NZB still parsing as failed and drops its source file and marker; any other NZB has nothing to release.
func (u *Usenet) Fail(id string, cause error) error {
	nzb, err := u.nzbStorage.GetNZBHeader(id)
	if errors.Is(err, customerror.ErrUsenetManifestMissing) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read NZB %s: %w", id, err)
	}
	if nzb.Status != NZBStatusParsing {
		return nil
	}
	return u.markAsFailed(nzb, cause)
}

func (u *Usenet) Delete(nzoID string) error {
	nzb, err := u.nzbStorage.GetNZBHeader(nzoID)
	if err != nil {
		return fmt.Errorf("failed to get NZB: %w", err)
	}

	// Delete NZB XML file from disk
	if nzb.Path != "" {
		if removeErr := os.Remove(nzb.Path); removeErr != nil && !os.IsNotExist(removeErr) {
			u.logger.Warn().Err(removeErr).Str("path", nzb.Path).Msg("Failed to delete NZB file from disk")
		}

		// Delete marker files
		processedMarker := nzb.Path + ".processed"
		_ = os.Remove(processedMarker)
		failedMarker := nzb.Path + ".failed"
		_ = os.Remove(failedMarker)
	}

	// Delete from file-based storage
	if deleteNZBErr := u.nzbStorage.DeleteNZB(nzoID); deleteNZBErr != nil {
		return fmt.Errorf("failed to delete NZB from storage: %w", deleteNZBErr)
	}
	return nil
}

// PendingNZB is an unmanaged NZB file claimed by the metadata-directory watcher.
type PendingNZB struct {
	Name    string
	Path    string
	Content []byte
}

// ClaimNewNZBs moves unmanaged NZB files out of the watched extension and
// returns them for submission to the shared active-download queue.
func (u *Usenet) ClaimNewNZBs() ([]PendingNZB, error) {
	entries, err := os.ReadDir(u.metadataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata dir: %w", err)
	}

	var pending []PendingNZB
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name, claimedPath, ok := u.claimNZB(entry.Name())
		if !ok {
			continue
		}

		content, readFileErr := os.ReadFile(claimedPath)
		if readFileErr != nil {
			u.logger.Error().Err(readFileErr).Str("path", claimedPath).Msg("Failed to read claimed NZB")
			continue
		}
		pending = append(pending, PendingNZB{Name: name, Path: claimedPath, Content: content})
	}

	if len(pending) > 0 {
		u.logger.Info().Int("count", len(pending)).Msg("Found new NZB files to queue")
	}
	return pending, nil
}

// claimNZB claims a watched entry by renaming it to "<name>.importing" so
// the watcher skips it next scan. An entry already claimed by an earlier,
// interrupted scan is reused. It returns the NZB name and the claimed path;
// ok is false for entries that are not unmanaged NZBs or cannot be claimed.
func (u *Usenet) claimNZB(entryName string) (string, string, bool) {
	if strings.HasSuffix(entryName, ".nzb.importing") {
		return strings.TrimSuffix(entryName, ".importing"), filepath.Join(u.metadataDir, entryName), true
	}
	if filepath.Ext(entryName) != ".nzb" {
		return "", "", false
	}
	path := filepath.Join(u.metadataDir, entryName)
	if fileExists(path+".processed") || fileExists(path+".processing") || fileExists(path+".failed") {
		return "", "", false
	}
	claimedPath := path + ".importing"
	if renameErr := os.Rename(path, claimedPath); renameErr != nil {
		// Skip this entry instead of aborting the whole scan. A single
		// poison file (e.g. a name so long that appending ".importing"
		// exceeds the filesystem limit) previously failed every refresh
		// and permanently blocked all other pending NZBs.
		if !os.IsNotExist(renameErr) {
			u.logger.Error().Err(renameErr).Str("name", entryName).Msg("Failed to claim NZB; skipping")
		}
		return "", "", false
	}
	return entryName, claimedPath, true
}

// RemoveClaimedNZB removes a watched source after it has been staged by the queue.
func (u *Usenet) RemoveClaimedNZB(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
