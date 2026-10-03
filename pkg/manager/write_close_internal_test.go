package manager

import (
	"errors"
	"io"
	"testing"
)

type closeFailingWriter struct {
	writes   int
	closed   bool
	closeErr error
}

func (w *closeFailingWriter) Write(p []byte) (int, error) {
	w.writes++
	return len(p), nil
}

func (w *closeFailingWriter) Close() error {
	w.closed = true
	return w.closeErr
}

// A download whose file fails to close may be truncated; it must not count
// as complete.
func TestWriteAndCloseReportsCloseFailure(t *testing.T) {
	t.Parallel()
	errDiskFull := errors.New("no space left on device")
	f := &closeFailingWriter{closeErr: errDiskFull}
	err := writeAndClose(f, func(w io.Writer) error {
		_, writeErr := w.Write([]byte("segment"))
		return writeErr
	})
	if !errors.Is(err, errDiskFull) || !f.closed || f.writes != 1 {
		t.Fatalf("err = %v, closed = %v, writes = %d", err, f.closed, f.writes)
	}

	errDownload := errors.New("article missing")
	f = &closeFailingWriter{}
	if err = writeAndClose(f, func(io.Writer) error { return errDownload }); !errors.Is(err, errDownload) || !f.closed {
		t.Fatalf("download failure: err = %v, closed = %v", err, f.closed)
	}

	f = &closeFailingWriter{}
	if err = writeAndClose(f, func(io.Writer) error { return nil }); err != nil || !f.closed {
		t.Fatalf("success: err = %v, closed = %v", err, f.closed)
	}
}
