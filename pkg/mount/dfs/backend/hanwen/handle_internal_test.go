//go:build linux || (darwin && amd64)

package hanwen

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"

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
	info := testRemoteFileInfo(t, 8192, time.Now())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fh := &Handle{
				info:       info,
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

func TestHandleReadFailureLogNamesTheFile(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	fh := &Handle{
		info:       testRemoteFileInfo(t, 8192, time.Now()),
		streamFile: &fakeStream{size: 8192, n: 0, err: io.ErrUnexpectedEOF},
		logger:     logger.NewRateLimitedLogger(logger.WithLogger(zerolog.New(&out))).Rate("test"),
	}
	if _, errno := fh.Read(context.Background(), make([]byte, 1024), 0); errno != syscall.EIO {
		t.Fatalf("errno = %v, want EIO", errno)
	}
	if !strings.Contains(out.String(), `"file":"test-entry/video.mkv"`) {
		t.Fatalf("log line does not name the file: %s", out.String())
	}
}
