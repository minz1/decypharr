//go:build linux || (darwin && amd64)

package hanwen

import (
	"context"
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/sirrobot01/decypharr/internal/logger"
)

type fakeStream struct {
	size int64
	n    int
	err  error
}

func (s *fakeStream) ReadAtContext(_ context.Context, p []byte, _ int64) (int, error) {
	for i := range p[:s.n] {
		p[i] = 0xAB
	}
	return s.n, s.err
}

func (s *fakeStream) Size() int64  { return s.size }
func (s *fakeStream) Close() error { return nil }

func TestHandleReadReportsFailedReadsInsideTheFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		off       int64
		n         int
		err       error
		wantErrno syscall.Errno
		wantBytes int
	}{
		{
			"truncated download at offset 0",
			0,
			0,
			fmt.Errorf("download failed: %w", io.ErrUnexpectedEOF),
			syscall.EIO,
			0,
		},
		{"short read mid-file", 0, 512, io.EOF, syscall.EIO, 0},
		{"short read mid-file without error", 0, 512, nil, syscall.EIO, 0},
		{"wrapped EOF mid-file", 4096, 0, fmt.Errorf("download failed: %w", io.EOF), syscall.EIO, 0},
		{"canceled read", 0, 0, fmt.Errorf("download failed: %w", context.Canceled), syscall.EINTR, 0},
		{"timed out read", 0, 0, fmt.Errorf("download failed: %w", context.DeadlineExceeded), syscall.ETIMEDOUT, 0},
		{"full read", 0, 1024, nil, 0, 1024},
		{"read ending at end of file", 8192 - 512, 512, io.EOF, 0, 512},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fh := &Handle{
				streamFile: &fakeStream{size: 8192, n: tc.n, err: tc.err},
				logger:     logger.NewRateLimitedLogger().Rate("test"),
			}
			res, errno := fh.Read(context.Background(), make([]byte, 1024), tc.off)
			if errno != tc.wantErrno {
				t.Fatalf("errno = %v, want %v", errno, tc.wantErrno)
			}
			if errno != 0 {
				return
			}
			data, _ := res.Bytes(nil)
			if len(data) != tc.wantBytes {
				t.Fatalf("read %d bytes, want %d", len(data), tc.wantBytes)
			}
		})
	}
}
