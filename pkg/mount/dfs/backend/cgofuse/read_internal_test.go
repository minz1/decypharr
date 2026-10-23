package cgofuse

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
	"github.com/winfsp/cgofuse/fuse"

	internalconfig "github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
	"github.com/sirrobot01/decypharr/pkg/storage"
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

func testRemoteFileInfo(t *testing.T, size int64) *manager.FileInfo {
	t.Helper()
	managerInstance, _ := managertest.New(t, nil)
	entry := &storage.Entry{
		Protocol: internalconfig.ProtocolTorrent,
		InfoHash: "test-hash",
		Name:     "test-entry",
		Size:     size,
		Files: map[string]*storage.File{
			"video.mkv": {Name: "video.mkv", Size: size, InfoHash: "test-hash", AddedOn: time.Now()},
		},
	}
	if err := managerInstance.Storage().AddOrUpdate(entry); err != nil {
		t.Fatalf("add test entry: %v", err)
	}
	info, err := managerInstance.GetTorrentFile(entry.Name, "video.mkv")
	if err != nil {
		t.Fatalf("get test file metadata: %v", err)
	}
	return info
}

func TestReadReportsFailedReadsInsideTheFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		off  int64
		n    int
		err  error
		want int
	}{
		{"truncated download at offset 0", 0, 0, fmt.Errorf("download failed: %w", io.ErrUnexpectedEOF), -fuse.EIO},
		{"short read mid-file", 0, 512, io.EOF, -fuse.EIO},
		{"short read mid-file without error", 0, 512, nil, -fuse.EIO},
		{"wrapped EOF mid-file", 4096, 0, fmt.Errorf("download failed: %w", io.EOF), -fuse.EIO},
		{"canceled read", 0, 0, fmt.Errorf("download failed: %w", context.Canceled), -fuse.EINTR},
		{"timed out read", 0, 0, fmt.Errorf("download failed: %w", context.DeadlineExceeded), -fuse.ETIMEDOUT},
		{"closed reader", 0, 0, syscall.EBADF, -fuse.EBADF},
		{"full read", 0, 1024, nil, 1024},
		{"read ending at end of file", 8192 - 512, 512, io.EOF, 512},
	}
	info := testRemoteFileInfo(t, 8192)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := NewFS(nil, nil, zerolog.Nop())
			fh := f.handles.Create(info, &fakeStream{size: 8192, n: tc.n, err: tc.err})
			if got := f.Read("/test-entry/video.mkv", make([]byte, 1024), tc.off, fh); got != tc.want {
				t.Fatalf("Read = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadFailureLogNamesTheFile(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	f := NewFS(nil, nil, zerolog.New(&out))
	fh := f.handles.Create(testRemoteFileInfo(t, 8192), &fakeStream{size: 8192, err: io.ErrUnexpectedEOF})
	if got := f.Read("/test-entry/video.mkv", make([]byte, 1024), 0, fh); got != -fuse.EIO {
		t.Fatalf("Read = %d, want EIO", got)
	}
	line := out.String()
	if !strings.Contains(line, `"file":"/test-entry/video.mkv"`) || !strings.Contains(line, "FUSE read failed") {
		t.Fatalf("log line does not name the file: %s", line)
	}
}
