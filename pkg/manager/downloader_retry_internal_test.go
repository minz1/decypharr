package manager

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

func TestLocalDownloaderRetriesServiceUnavailable(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("resumable-download"), 4096)
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if r.Method == http.MethodHead {
			return
		}
		if gets.Add(1) == 1 {
			http.Error(w, "temporary CDN outage", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	destination := filepath.Join(t.TempDir(), "release.mkv")
	var downloaded atomic.Int64
	d := &Downloader{
		manager: withTestConfig(t, &Manager{
			ctx:          t.Context(),
			streamClient: server.Client(),
		}),
		logger: zerolog.Nop(),
	}
	if err := d.localDownloader(server.URL, destination, nil, func(delta, _ int64) {
		downloaded.Add(delta)
	}); err != nil {
		t.Fatalf("localDownloader() error = %v", err)
	}

	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(payload))
	}
	if gets.Load() != 2 {
		t.Fatalf("GET requests = %d, want one retry", gets.Load())
	}
	if downloaded.Load() != int64(len(payload)) {
		t.Fatalf("reported bytes = %d, want %d", downloaded.Load(), len(payload))
	}
}

func TestLocalDownloaderResumesAfterUnexpectedEOF(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("range-resume"), 8192)
	cut := len(payload) / 2
	var (
		gets        atomic.Int32
		resumeRange atomic.Value
	)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		headers := make(http.Header)
		headers.Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: int64(len(payload)),
				Header:        headers,
				Body:          http.NoBody,
				Request:       r,
			}, nil
		}

		if gets.Add(1) == 1 {
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: int64(len(payload)),
				Header:        headers,
				Body: io.NopCloser(io.MultiReader(
					bytes.NewReader(payload[:cut]),
					unexpectedEOFReader{},
				)),
				Request: r,
			}, nil
		}

		resumeRange.Store(r.Header.Get("Range"))
		headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", cut, len(payload)-1, len(payload)))
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			ContentLength: int64(len(payload) - cut),
			Header:        headers,
			Body:          io.NopCloser(bytes.NewReader(payload[cut:])),
			Request:       r,
		}, nil
	})}

	destination := filepath.Join(t.TempDir(), "release.mkv")
	var downloaded atomic.Int64
	d := &Downloader{
		manager: withTestConfig(t, &Manager{
			ctx:          t.Context(),
			streamClient: client,
		}),
		logger: zerolog.Nop(),
	}
	if err := d.localDownloader("https://cdn.example/release.mkv", destination, nil, func(delta, _ int64) {
		downloaded.Add(delta)
	}); err != nil {
		t.Fatalf("localDownloader() error = %v", err)
	}

	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(payload))
	}
	if gets.Load() != 2 {
		t.Fatalf("GET requests = %d, want one resumed retry", gets.Load())
	}
	if gotRange, _ := resumeRange.Load().(string); gotRange != fmt.Sprintf("bytes=%d-", cut) {
		t.Fatalf("resume Range = %q, want bytes=%d-", gotRange, cut)
	}
	if downloaded.Load() != int64(len(payload)) {
		t.Fatalf("reported bytes = %d, want %d", downloaded.Load(), len(payload))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

// A torrent download through grab gets the shared file mode, like every
// other file the arr imports, not grab's 0666.
func TestLocalDownloaderUsesSharedFileMode(t *testing.T) {
	t.Parallel()
	payload := []byte("movie bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	m := withTestConfig(t, &Manager{ctx: t.Context(), streamClient: server.Client()})
	d := &Downloader{manager: m, logger: zerolog.Nop()}
	destination := filepath.Join(dir, "release.mkv")
	if err := d.localDownloader(server.URL, destination, nil, nil); err != nil {
		t.Fatal(err)
	}

	// The same mode and umask applied by os.OpenFile, as the other paths do.
	probe := filepath.Join(dir, "probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, m.config.SharedFileModeValue())
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	want, _ := os.Stat(probe)
	got, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode().Perm() != want.Mode().Perm() {
		t.Fatalf("downloaded file mode = %o, want %o", got.Mode().Perm(), want.Mode().Perm())
	}
}
