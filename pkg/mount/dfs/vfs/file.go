package vfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync/atomic"
	"syscall"
)

// File is the read interface exposed to FUSE backends. Both the disk-cached
// path (StreamingFile) and the direct-network path (DirectStreamFile) satisfy it.
type File interface {
	ReadAtContext(ctx context.Context, p []byte, off int64) (int, error)
	Size() int64
	Close() error
}

// StreamingFile is the FUSE file interface for VFS.
type StreamingFile struct {
	item     *CacheItem
	fileSize int64
	closed   atomic.Bool
	// direct serves this handle's reads once the cache went over budget
	// under it. GetFile only picks the direct path at open time, and the
	// kernel never reopens a live handle.
	direct atomic.Pointer[DirectStreamFile]
}

// NewStreamingFile creates a new streaming file handle. It returns nil when
// the item has been claimed for teardown by the cache janitor — the caller
// must fetch a fresh item and try again (see Manager.GetFile).
func NewStreamingFile(item *CacheItem) *StreamingFile {
	if !item.Open() { // take an open reference; fails on a claimed item
		return nil
	}

	return &StreamingFile{
		item:     item,
		fileSize: item.info.Size,
	}
}

// ReadAt implements [io.ReaderAt] using a background context.
// Prefer ReadAtContext when a caller context is available (e.g. from a FUSE handle).
func (f *StreamingFile) ReadAt(p []byte, off int64) (int, error) {
	return f.ReadAtContext(context.Background(), p, off)
}

// ReadAtContext reads from the file, passing ctx into the download layer so
// the operation can be interrupted by a read timeout or client disconnect.
func (f *StreamingFile) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, fs.ErrClosed
	}
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= f.fileSize {
		return 0, io.EOF
	}

	readSize := int64(len(p))
	if readSize > f.fileSize-off {
		readSize = f.fileSize - off
		p = p[:readSize]
	}

	var n int
	var err error
	if direct := f.direct.Load(); direct != nil {
		n, err = direct.ReadAtContext(ctx, p, off)
	} else {
		n, err = f.item.ReadAtContext(ctx, p, off)
		if errors.Is(err, syscall.ENOSPC) {
			// The cache stopped taking writes (over budget or disk full):
			// this read and every later one stream directly.
			// ponytail: the item keeps its open reference until Close, so its
			// cached bytes stay unevictable while this handle streams directly.
			if f.direct.CompareAndSwap(nil, f.newDirect()) {
				f.item.cache.logger.Info().
					Str("file", f.item.key).
					Msg("cache refused writes mid-read, serving direct")
			}
			n, err = f.direct.Load().ReadAtContext(ctx, p, off)
		}
	}

	if n < int(readSize) && err == nil {
		err = io.EOF
	}
	return n, err
}

// newDirect builds the direct-network reader for this handle's file, the
// same reader GetFile opens when the cache is already over budget.
func (f *StreamingFile) newDirect() *DirectStreamFile {
	cache := f.item.cache
	return &DirectStreamFile{
		src:      cache.manager,
		entry:    f.item.entry,
		filename: f.item.filename,
		size:     f.fileSize,
		retries:  cache.config.Retries,
	}
}

// Size returns the file size.
func (f *StreamingFile) Size() int64 {
	return f.fileSize
}

// Close closes the file handle.
func (f *StreamingFile) Close() error {
	if f.closed.Swap(true) {
		return nil
	}
	f.item.Release()
	return nil
}
