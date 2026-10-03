package fs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirrobot01/decypharr/pkg/usenet/fs/reader"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

// File is one virtual volume opened from an [FS].
type File struct {
	readerSettings

	volume          *types.Volume
	info            volumeInfo
	reader          io.ReadCloser                          // Sequential reader (for Read() method)
	streamingReader atomic.Pointer[reader.StreamingReader] // Streaming reader for ReadAt()
	readerOnce      sync.Once                              // Ensures streaming reader created exactly once
	readerErr       error                                  // Error from streaming reader creation
	pos             atomic.Int64
	closed          atomic.Bool
}

func (vf *File) Read(p []byte) (int, error) {
	if vf.closed.Load() {
		return 0, fs.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	curPos := vf.pos.Load()
	if curPos >= vf.volume.Size {
		return 0, io.EOF
	}
	remaining := vf.volume.Size - curPos
	if remaining <= 0 {
		return 0, io.EOF
	}
	err := vf.ensureReader() // Pass the current position to ensureReader
	if err != nil {
		return 0, err
	}
	readLen := len(p)
	if int64(readLen) > remaining {
		readLen = int(remaining)
	}
	n, readErr := vf.reader.Read(p[:readLen])
	vf.pos.Add(int64(n))
	atEOF := errors.Is(readErr, io.EOF)
	if atEOF || vf.pos.Load() >= vf.volume.Size {
		vf.closeSequentialReader()
	}
	switch {
	case readErr != nil && !atEOF:
		return n, readErr
	case atEOF || n < readLen:
		return n, io.EOF
	}
	return n, nil
}

// closeSequentialReader drops the Read() reader; the next Read reopens at pos.
func (vf *File) closeSequentialReader() {
	if vf.reader != nil {
		_ = vf.reader.Close()
		vf.reader = nil
	}
}

func (vf *File) ReadAt(p []byte, off int64) (int, error) {
	if vf.closed.Load() {
		return 0, fs.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("rar: negative read offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= vf.volume.Size {
		return 0, io.EOF
	}

	remaining := vf.volume.Size - off
	if remaining <= 0 {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	eofAfter := false
	if toRead > remaining {
		toRead = remaining
		eofAfter = true
	}

	// Use streaming reader
	reader := vf.getOrCreateStreamingReader()
	if reader == nil {
		return 0, fmt.Errorf("failed to create streaming reader for volume %s: %w", vf.volume.Name, vf.readerErr)
	}
	n, readErr := reader.ReadAt(p[:int(toRead)], off)

	if readErr != nil {
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			return n, io.EOF
		}
		return n, readErr
	}
	if eofAfter {
		return n, io.EOF
	}
	return n, nil
}

// getOrCreateStreamingReader returns the streaming reader, creating it if needed.
// Uses [sync.Once] to ensure exactly one reader is created even with concurrent calls.
func (vf *File) getOrCreateStreamingReader() *reader.StreamingReader {
	vf.readerOnce.Do(func() {
		r, err := vf.newReader(vf.volume)
		if err != nil {
			vf.readerErr = err
			vf.logger.Error().Err(err).Msg("Failed to create streaming reader")
			return
		}
		vf.streamingReader.Store(r)
	})

	return vf.streamingReader.Load()
}

func (vf *File) Seek(offset int64, whence int) (int64, error) {
	if vf.closed.Load() {
		return 0, fs.ErrClosed
	}

	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = vf.pos.Load() + offset
	case io.SeekEnd:
		newPos = vf.volume.Size + offset
	default:
		return 0, fmt.Errorf("rar: invalid seek whence %d", whence)
	}

	if newPos < 0 {
		return 0, fmt.Errorf("rar: seek before beginning of file")
	}
	if newPos > vf.volume.Size {
		newPos = vf.volume.Size
	}
	if newPos != vf.pos.Load() {
		vf.closeSequentialReader()
	}
	vf.pos.Store(newPos)
	return vf.pos.Load(), nil
}

// newReaderAt creates a sequential reader positioned at start.
func (vf *File) newReaderAt(start int64) (io.ReadCloser, error) {
	r, err := vf.newReader(vf.volume)
	if err != nil {
		return nil, fmt.Errorf("failed to create streaming reader: %w", err)
	}
	if start > 0 {
		if _, seekErr := r.Seek(start, io.SeekStart); seekErr != nil {
			_ = r.Close()
			return nil, fmt.Errorf("failed to seek to start position: %w", seekErr)
		}
	}
	return r, nil
}

func (vf *File) Write(_ []byte) (int, error) {
	if vf.closed.Load() {
		return 0, fs.ErrClosed
	}
	return 0, fmt.Errorf("rar: write not supported on read-only Volume")
}

func (vf *File) ensureReader() error {
	if vf.reader != nil {
		return nil
	}
	reader, err := vf.newReaderAt(vf.pos.Load())
	if err != nil {
		return err
	}
	vf.reader = reader
	return nil
}

func (vf *File) Stat() (fs.FileInfo, error) {
	return vf.info, nil
}

func (vf *File) Close() error {
	if vf.closed.Swap(true) {
		return nil
	}

	// Close streaming reader if used
	if vf.streamingReader.Load() != nil {
		_ = vf.streamingReader.Load().Close()
		vf.streamingReader.Store(nil)
	}

	// Close sequential reader
	if vf.reader != nil {
		err := vf.reader.Close()
		vf.reader = nil
		return err
	}
	return nil
}

// readOnlyMode is the permission bits of every virtual volume.
const readOnlyMode fs.FileMode = 0o444

type volumeInfo struct {
	name string
	size int64
}

func (vi volumeInfo) Name() string       { return vi.name }
func (vi volumeInfo) Size() int64        { return vi.size }
func (vi volumeInfo) Mode() fs.FileMode  { return readOnlyMode }
func (vi volumeInfo) ModTime() time.Time { return time.Time{} }
func (vi volumeInfo) IsDir() bool        { return false }
func (vi volumeInfo) Sys() any           { return nil }
