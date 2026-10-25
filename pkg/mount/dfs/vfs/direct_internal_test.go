package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/link"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// fakeStreamSource implements streamSource using a real [httptest.Server] so we
// exercise HTTP range-request handling without the full manager stack.
type fakeStreamSource struct {
	srv *httptest.Server
	// content is the synthetic file served by srv.
	content []byte
}

func newFakeStreamSource(t *testing.T, content []byte) *fakeStreamSource {
	t.Helper()
	src := &fakeStreamSource{content: content}
	src.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if start < 0 || end >= int64(len(src.content)) || start > end {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(src.content[start : end+1])
	}))
	t.Cleanup(src.srv.Close)
	return src
}

func (s *fakeStreamSource) OpenStream(
	ctx context.Context,
	_ *storage.Entry,
	_ string,
	offset int64,
	_ string,
) (manager.StreamReader, error) {
	end := int64(len(s.content)) - 1
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.srv.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return &fakeStreamReader{body: resp.Body, size: int64(len(s.content)) - offset}, nil
}

// fakeStreamReader adapts an [http.Response] body to manager.StreamReader.
// Seek is unused by DirectStreamFile (each read opens a fresh stream at the
// requested offset) so it is left unimplemented.
type fakeStreamReader struct {
	body io.ReadCloser
	size int64
}

func (r *fakeStreamReader) Read(p []byte) (int, error) { return r.body.Read(p) }
func (r *fakeStreamReader) Close() error               { return r.body.Close() }
func (r *fakeStreamReader) Size() int64                { return r.size }
func (r *fakeStreamReader) Prime() error               { return nil }
func (r *fakeStreamReader) Seek(int64, int) (int64, error) {
	return 0, errors.New("seek not supported by fakeStreamReader")
}

func TestDirectStreamFile_ReadAtContext(t *testing.T) {
	t.Parallel()
	content := make([]byte, 1024)
	for i := range content {
		content[i] = byte(i % 251)
	}

	src := newFakeStreamSource(t, content)
	entry := &storage.Entry{
		Files: map[string]*storage.File{
			"test.bin": {Name: "test.bin", Size: int64(len(content))},
		},
	}
	f := &DirectStreamFile{
		src:      src,
		entry:    entry,
		filename: "test.bin",
		size:     int64(len(content)),
		retries:  2,
	}

	t.Run("full read", func(t *testing.T) {
		t.Parallel()
		buf := make([]byte, len(content))
		n, err := f.ReadAtContext(context.Background(), buf, 0)
		if err != nil {
			t.Fatalf("ReadAtContext: %v", err)
		}
		if n != len(content) || !bytes.Equal(buf, content) {
			t.Fatalf("got %d bytes %v, want %v", n, buf, content)
		}
	})

	t.Run("mid-file range", func(t *testing.T) {
		t.Parallel()
		const off = 100
		buf := make([]byte, 200)
		n, err := f.ReadAtContext(context.Background(), buf, off)
		if err != nil {
			t.Fatalf("ReadAtContext: %v", err)
		}
		if n != len(buf) || !bytes.Equal(buf, content[off:off+len(buf)]) {
			t.Fatalf("got %d bytes %v, want content[%d:%d]", n, buf, off, off+len(buf))
		}
	})

	t.Run("no disk writes", func(t *testing.T) {
		t.Parallel()
		// CacheDir is never touched by DirectStreamFile — verified structurally:
		// newDirectStreamFile takes a streamSource, not a cache, so there is
		// no code path from ReadAtContext to any disk write.
		// This test confirms the read succeeds without needing a CacheDir at all.
		f2 := &DirectStreamFile{
			src:      src,
			entry:    entry,
			filename: "test.bin",
			size:     int64(len(content)),
			retries:  0,
		}
		buf := make([]byte, 10)
		if _, err := f2.ReadAtContext(context.Background(), buf, 0); err != nil {
			t.Fatalf("ReadAtContext without CacheDir: %v", err)
		}
	})
}

func TestDirectStreamFile_RetriesOnTransientFailure(t *testing.T) {
	t.Parallel()
	const fileSize = 64
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i)
	}

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// First call: simulate transient 500
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	}))
	defer srv.Close()

	retryingSrc := &fakeStreamSource{content: content, srv: srv}
	entry := &storage.Entry{
		Files: map[string]*storage.File{
			"test.bin": {Name: "test.bin", Size: fileSize},
		},
	}
	f := &DirectStreamFile{
		src:      retryingSrc,
		entry:    entry,
		filename: "test.bin",
		size:     fileSize,
		retries:  2,
	}

	buf := make([]byte, fileSize)
	if _, err := f.ReadAtContext(context.Background(), buf, 0); err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 calls (1 failure + 1 success), got %d", calls)
	}
}

func TestDirectStreamFile_RetriesTruncatedBody(t *testing.T) {
	t.Parallel()
	const fileSize = 64
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i + 1)
	}

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var start, end int64
		_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		w.WriteHeader(http.StatusPartialContent)
		if calls == 1 {
			end = start + 9
		}
		_, _ = w.Write(content[start : end+1])
	}))
	defer srv.Close()

	f := &DirectStreamFile{
		src:      &fakeStreamSource{content: content, srv: srv},
		entry:    &storage.Entry{Files: map[string]*storage.File{"test.bin": {Name: "test.bin", Size: fileSize}}},
		filename: "test.bin",
		size:     fileSize,
		retries:  2,
	}

	buf := make([]byte, 32)
	n, err := f.ReadAtContext(context.Background(), buf, 8)
	if err != nil || n != len(buf) {
		t.Fatalf("ReadAtContext = (%d, %v), want (%d, nil)", n, err, len(buf))
	}
	if !bytes.Equal(buf, content[8:40]) {
		t.Fatalf("read %v, want %v", buf, content[8:40])
	}
}

func TestDirectStreamFile_FailsWhenEveryBodyIsTruncated(t *testing.T) {
	t.Parallel()
	const fileSize = 64
	content := make([]byte, fileSize)

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var start, end int64
		_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : start+10])
	}))
	defer srv.Close()

	f := &DirectStreamFile{
		src:      &fakeStreamSource{content: content, srv: srv},
		entry:    &storage.Entry{Files: map[string]*storage.File{"test.bin": {Name: "test.bin", Size: fileSize}}},
		filename: "test.bin",
		size:     fileSize,
		retries:  2,
	}

	n, err := f.ReadAtContext(context.Background(), make([]byte, 32), 8)
	if n != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadAtContext = (%d, %v), want (0, %v)", n, err, io.ErrUnexpectedEOF)
	}
	if calls != 3 {
		t.Fatalf("server saw %d requests, want 3", calls)
	}
}

// permanentSource fails every open (or first read) with a permanent error.
type permanentSource struct {
	onRead bool
	opens  atomic.Int32
}

var errEntryBad = link.NewPermanentError(errors.New("marked as bad"), "entry_bad")

func (s *permanentSource) OpenStream(
	context.Context, *storage.Entry, string, int64, string,
) (manager.StreamReader, error) {
	s.opens.Add(1)
	if s.onRead {
		return &fakeStreamReader{body: io.NopCloser(iotest.ErrReader(errEntryBad))}, nil
	}
	return nil, errEntryBad
}

func TestDirectStreamFileDoesNotRetryPermanentErrors(t *testing.T) {
	t.Parallel()
	for _, onRead := range []bool{false, true} {
		src := &permanentSource{onRead: onRead}
		f := &DirectStreamFile{src: src, filename: "test.bin", size: 1024, retries: 3}
		if _, err := f.ReadAtContext(t.Context(), make([]byte, 16), 0); !errors.Is(err, errEntryBad) {
			t.Fatalf("onRead=%v: err = %v, want the permanent error", onRead, err)
		}
		if got := src.opens.Load(); got != 1 {
			t.Errorf("onRead=%v: %d opens, want 1 (no retry of a permanent error)", onRead, got)
		}
	}
}

// failingSource fails every open with err.
type failingSource struct {
	err   error
	opens atomic.Int32
}

func (s *failingSource) OpenStream(
	context.Context, *storage.Entry, string, int64, string,
) (manager.StreamReader, error) {
	s.opens.Add(1)
	return nil, s.err
}

func TestDirectStreamFileRetriesUntypedErrorWithPortLikeStatus(t *testing.T) {
	t.Parallel()
	reset := &net.OpError{
		Op:     "read",
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 40412},
		Addr:   &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 443},
		Err:    syscall.ECONNRESET,
	}
	src := &failingSource{err: reset}
	f := &DirectStreamFile{src: src, filename: "test.bin", size: 1024, retries: 2}
	if _, err := f.ReadAtContext(t.Context(), make([]byte, 16), 0); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("err = %v, want the connection reset", err)
	}
	if got := src.opens.Load(); got != 3 {
		t.Errorf("%d opens of %q, want 3 (a reset is transient)", got, reset)
	}
}

// A provider's 404 for the link will not change on a retry, so the read stops after one open.
func TestDirectStreamFileStopsOnPermanentProviderError(t *testing.T) {
	t.Parallel()
	gone := customerror.NewProviderStatusError(errors.New("realdebrid API error: Status: 404 || Code: 7"), 404)
	src := &failingSource{err: fmt.Errorf("get download link for realdebrid: %w", errors.Join(gone))}
	f := &DirectStreamFile{src: src, filename: "test.bin", size: 1024, retries: 2}
	if _, err := f.ReadAtContext(t.Context(), make([]byte, 16), 0); !errors.Is(err, gone) {
		t.Fatalf("err = %v, want the provider error", err)
	}
	if got := src.opens.Load(); got != 1 {
		t.Errorf("%d opens, want 1 (the provider refused the link for good)", got)
	}
}
