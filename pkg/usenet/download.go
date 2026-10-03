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
	// Buffered so fetching can run ahead of the in-order writer.
	results := make(chan segmentResult, workers*downloadResultsPerWorker)
	ordered := &orderedSegmentWriter{
		w:        writer,
		pending:  make(map[int][]byte),
		progress: progressCallback,
		workers:  int64(workers),
	}
	var writerWg sync.WaitGroup
	writerWg.Go(func() { ordered.run(results) })

	p := pool.New().WithContext(ctx).WithMaxGoroutines(workers)
	for idx, segment := range file.Segments {
		p.Go(func(ctx context.Context) error {
			if writeErr := ordered.err(); writeErr != nil {
				return writeErr
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// A failed segment is reported through results; other workers
			// keep going until the writer records it.
			results <- u.fetchDownloadSegment(ctx, idx, segment)
			return nil
		})
	}

	fetchErr := p.Wait()
	close(results)
	writerWg.Wait()

	if writeErr := ordered.err(); writeErr != nil {
		return writeErr
	}
	if fetchErr != nil {
		return fetchErr
	}

	u.logger.Info().
		Str("file", filename).
		Int64("bytes", ordered.downloaded).
		Msg("Download complete")
	return nil
}

// downloadResultsPerWorker sizes the result buffer per fetch worker.
const downloadResultsPerWorker = 2

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
// out-of-order arrivals until their predecessors land.
type orderedSegmentWriter struct {
	w        io.Writer
	pending  map[int][]byte
	next     int
	progress ProgressCallback
	workers  int64

	written    int64 // segments written; owned by run
	downloaded int64 // bytes written; read after run returns

	mu       sync.Mutex
	firstErr error
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
	for {
		data, ok := o.pending[o.next]
		if !ok {
			return
		}
		delete(o.pending, o.next)
		n, err := o.w.Write(data)
		if err != nil {
			o.setErr(fmt.Errorf("write failed at segment %d: %w", o.next, err))
			return
		}
		o.written++
		o.downloaded += int64(n)
		o.next++
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
