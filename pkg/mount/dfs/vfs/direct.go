package vfs

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// streamSource is the narrow interface DirectStreamFile needs from manager.Manager.
// Keeping it minimal lets tests inject a fake without wiring the full manager stack.
type streamSource interface {
	Stream(ctx context.Context, entry *storage.Entry, filename string, start, end int64, writer io.Writer, onReady manager.StreamReadyFunc, client string) error
}

// DirectStreamFile serves reads straight from the debrid network without
// writing to disk. Used when the cache is at capacity so new file opens
// degrade gracefully instead of returning EIO once the partition fills.
type DirectStreamFile struct {
	src      streamSource
	entry    *storage.Entry
	filename string
	size     int64
	retries  int
	closed   atomic.Bool
}

func newDirectStreamFile(mgr *manager.Manager, entry *storage.Entry, filename string, size int64, retries int) *DirectStreamFile {
	return &DirectStreamFile{
		src:      mgr,
		entry:    entry,
		filename: filename,
		size:     size,
		retries:  retries,
	}
}

func (f *DirectStreamFile) Size() int64 { return f.size }

func (f *DirectStreamFile) Close() error {
	f.closed.Swap(true)
	return nil
}

// ReadAtContext fetches [off, off+len(p)) directly from the debrid network,
// retrying up to f.retries times on transient failures. The context deadline
// (ReadTimeout on the FUSE side) acts as the hard cap.
func (f *DirectStreamFile) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if off >= f.size {
		return 0, io.EOF
	}

	end := off + int64(len(p)) - 1
	if end >= f.size {
		end = f.size - 1
		p = p[:f.size-off]
	}

	var lastErr error
	for attempt := 0; attempt <= f.retries; attempt++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 150 * time.Millisecond):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}

		w := &fixedWriter{dst: p}
		err := f.src.Stream(ctx, f.entry, f.filename, off, end, w, nil, "DFS-direct")
		if err == nil {
			return w.n, nil
		}
		lastErr = err
	}
	return 0, lastErr
}

// fixedWriter writes into a caller-owned slice, stopping when full.
// manager.Stream requests exactly the byte range we need, so overflow
// should not occur in practice.
type fixedWriter struct {
	dst []byte
	n   int
}

func (w *fixedWriter) Write(data []byte) (int, error) {
	written := copy(w.dst[w.n:], data)
	w.n += written
	return written, nil
}
