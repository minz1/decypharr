package usenet

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sourcegraph/conc/pool"

	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// segmentResult holds a fetched segment and its index for ordered writing.
type segmentResult struct {
	index int
	data  []byte
	err   error
}

// ProgressCallback is called periodically during download with progress info
// downloaded: total bytes written so far, speed: bytes per second (estimated)
type ProgressCallback func(downloaded int64, speed int64)

// Download downloads a file by fetching segments in parallel and streaming to writer in order.
// Bytes flow to the writer progressively as in-order segments complete - no waiting for all segments.
// If progressCallback is provided, it will be called after each segment write with current progress.
func (u *Usenet) Download(
	ctx context.Context,
	nzoID, filename string,
	writer io.Writer,
	progressCallback ProgressCallback,
) error {
	file, err := u.getFile(nzoID, filename)
	if err != nil {
		return fmt.Errorf("failed to get file: %w", err)
	}
	if len(file.Segments) == 0 {
		return fmt.Errorf("file has no segments: %s", file.Name)
	}
	if file.IsEncrypted {
		// Raw segments hold AES ciphertext; only the streaming reader
		// decrypts, so route encrypted archive members through it.
		return u.downloadDecrypted(ctx, file, writer, progressCallback)
	}

	workers := max(u.processingMaxConnections, 1)
	downloaded, err := downloadSegments(ctx, file.Segments, workers, writer, progressCallback, u.fetchDownloadSegment)
	if err != nil {
		return err
	}

	u.logger.Info().
		Str("file", filename).
		Int64("bytes", downloaded).
		Msg("Download complete")
	return nil
}

const (
	// downloadResultsPerWorker sizes the result buffer per fetch worker.
	downloadResultsPerWorker = 2
	// downloadWindowPerWorker bounds how far fetching may run ahead of the
	// next segment to write, per worker. Out-of-order segments wait in RAM,
	// so this caps a download's memory however slow one segment is.
	downloadWindowPerWorker = 4
)

type segmentFetcher func(ctx context.Context, idx int, seg storage.NZBSegment) segmentResult

// downloadSegments fetches segments with workers in parallel and writes them
// to writer in order. It returns the bytes written.
func downloadSegments(
	ctx context.Context,
	segments []storage.NZBSegment,
	workers int,
	writer io.Writer,
	progressCallback ProgressCallback,
	fetch segmentFetcher,
) (int64, error) {
	// Buffered so fetching can run ahead of the in-order writer.
	results := make(chan segmentResult, workers*downloadResultsPerWorker)
	ordered := newOrderedSegmentWriter(writer, progressCallback, workers, workers*downloadWindowPerWorker)
	var writerWg sync.WaitGroup
	writerWg.Go(func() { ordered.run(results) })

	p := pool.New().WithContext(ctx).WithMaxGoroutines(workers)
	for idx, segment := range segments {
		p.Go(func(ctx context.Context) error {
			if waitErr := ordered.waitTurn(ctx, idx); waitErr != nil {
				return waitErr
			}
			// A failed segment is reported through results; other workers
			// keep going until the writer records it.
			results <- fetch(ctx, idx, segment)
			return nil
		})
	}

	fetchErr := p.Wait()
	close(results)
	writerWg.Wait()

	if writeErr := ordered.err(); writeErr != nil {
		return ordered.downloaded, writeErr
	}
	return ordered.downloaded, fetchErr
}

// fetchDownloadSegment fetches one segment through provider failover and
// trims it to the bytes this file uses.
func (u *Usenet) fetchDownloadSegment(ctx context.Context, idx int, seg storage.NZBSegment) segmentResult {
	var data []byte
	err := u.nntp.ExecuteWithFailover(ctx, nntp.WorkloadDownload, func(conn *nntp.Connection) error {
		d, e := conn.GetDecodedBody(seg.MessageID)
		data = d
		return e
	})
	if err != nil {
		return segmentResult{index: idx, err: fmt.Errorf("segment %d: %w", idx, err)}
	}
	// Handle SegmentDataStart for sliced segments
	if seg.SegmentDataStart > 0 {
		if seg.SegmentDataStart >= int64(len(data)) {
			return segmentResult{index: idx, err: fmt.Errorf("segment %d: offset exceeds data", idx)}
		}
		data = data[seg.SegmentDataStart:]
	}
	if int64(len(data)) > seg.Bytes {
		data = data[:seg.Bytes]
	}
	return segmentResult{index: idx, data: data}
}

// orderedSegmentWriter writes fetched segments in index order, holding
// out-of-order arrivals until their predecessors land. Fetchers wait their
// turn so at most window segments are fetched past the next one to write.
type orderedSegmentWriter struct {
	w        io.Writer
	pending  map[int][]byte
	progress ProgressCallback
	workers  int64
	window   int

	written    int64 // segments written; owned by run
	downloaded int64 // bytes written; read after run returns

	mu       sync.Mutex
	next     int           // next segment to write
	advanced chan struct{} // closed when next moves or an error is set
	firstErr error
}

func newOrderedSegmentWriter(w io.Writer, progress ProgressCallback, workers, window int) *orderedSegmentWriter {
	return &orderedSegmentWriter{
		w:        w,
		pending:  make(map[int][]byte),
		progress: progress,
		workers:  int64(workers),
		window:   max(window, 1),
		advanced: make(chan struct{}),
	}
}

func (o *orderedSegmentWriter) err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.firstErr
}

func (o *orderedSegmentWriter) setErr(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.firstErr == nil {
		o.firstErr = err
		o.wakeLocked()
	}
}

// wakeLocked releases every waitTurn caller to re-check. Caller holds o.mu.
func (o *orderedSegmentWriter) wakeLocked() {
	close(o.advanced)
	o.advanced = make(chan struct{})
}

// waitTurn blocks until segment idx is within the window past the next
// segment to write. It returns the first write or fetch error, or ctx's.
func (o *orderedSegmentWriter) waitTurn(ctx context.Context, idx int) error {
	for {
		o.mu.Lock()
		next, err, advanced := o.next, o.firstErr, o.advanced
		o.mu.Unlock()
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if idx < next+o.window {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-advanced:
		}
	}
}

func (o *orderedSegmentWriter) run(results <-chan segmentResult) {
	for result := range results {
		if result.err != nil {
			o.setErr(result.err)
			continue
		}
		o.pending[result.index] = result.data
		o.flush()
	}
}

// flush writes every consecutive pending segment starting at next.
func (o *orderedSegmentWriter) flush() {
	o.mu.Lock()
	next := o.next
	o.mu.Unlock()
	for {
		data, ok := o.pending[next]
		if !ok {
			return
		}
		delete(o.pending, next)
		n, err := o.w.Write(data)
		if err != nil {
			o.setErr(fmt.Errorf("write failed at segment %d: %w", next, err))
			return
		}
		o.written++
		o.downloaded += int64(n)
		next++
		o.mu.Lock()
		o.next = next
		o.wakeLocked()
		o.mu.Unlock()
		if o.progress != nil {
			// Rough speed estimate: assume ~1s per segment batch.
			o.progress(o.downloaded, o.downloaded/max(1, o.written)*o.workers)
		}
	}
}

// downloadDecrypted copies a whole file through the decrypting reader stack.
// ponytail: sequential, reader read-ahead only; the parallel raw path above is
// faster, so extend it with CBC decryption if encrypted downloads matter.
func (u *Usenet) downloadDecrypted(
	ctx context.Context,
	file *storage.NZBFile,
	writer io.Writer,
	progressCallback ProgressCallback,
) error {
	entry, err := u.createEntry(file, u.prefetchSize, RetentionWindow)
	if err != nil {
		return err
	}
	defer entry.cleanup()
	readerAt, size, err := entry.getOrCreateReader()
	if err != nil {
		return err
	}
	cursor := readerAt.OpenCursor()
	defer cursor.Close()

	dst := &progressWriter{w: writer, callback: progressCallback, start: time.Now()}
	return safeCopyBuffer(ctx, dst, newContextSectionReader(ctx, cursor, 0, size), nil)
}

// progressWriter reports cumulative bytes and average throughput per write.
type progressWriter struct {
	w        io.Writer
	callback ProgressCallback
	start    time.Time
	written  int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	if p.callback != nil && n > 0 {
		elapsed := max(time.Since(p.start), time.Millisecond)
		p.callback(p.written, int64(float64(p.written)/elapsed.Seconds()))
	}
	return n, err
}
