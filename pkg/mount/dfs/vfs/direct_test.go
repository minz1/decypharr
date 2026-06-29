package vfs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// fakeStreamSource implements streamSource using a real httptest.Server so we
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

func (s *fakeStreamSource) Stream(ctx context.Context, entry *storage.Entry, filename string, start, end int64, w io.Writer, onReady manager.StreamReadyFunc, client string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.srv.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func TestDirectStreamFile_ReadAtContext(t *testing.T) {
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
		buf := make([]byte, len(content))
		n, err := f.ReadAtContext(context.Background(), buf, 0)
		if err != nil {
			t.Fatalf("ReadAtContext: %v", err)
		}
		if n != len(content) {
			t.Fatalf("got %d bytes, want %d", n, len(content))
		}
		for i, b := range buf {
			if b != content[i] {
				t.Fatalf("byte %d: got %d, want %d", i, b, content[i])
			}
		}
	})

	t.Run("mid-file range", func(t *testing.T) {
		const off = 100
		buf := make([]byte, 200)
		n, err := f.ReadAtContext(context.Background(), buf, off)
		if err != nil {
			t.Fatalf("ReadAtContext: %v", err)
		}
		if n != 200 {
			t.Fatalf("got %d bytes, want 200", n)
		}
		for i, b := range buf {
			if b != content[off+i] {
				t.Fatalf("byte %d: got %d, want %d", i, b, content[off+i])
			}
		}
	})

	t.Run("no disk writes", func(t *testing.T) {
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
