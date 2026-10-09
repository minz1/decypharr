package vfs

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/manager"
	dfsconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// bytesBackend serves one in-memory file as the debrid origin.
type bytesBackend struct {
	entry   *storage.Entry
	content []byte
}

func (b *bytesBackend) GetEntryByName(string, string) (*storage.Entry, error) { return b.entry, nil }
func (b *bytesBackend) TrackStream(*storage.Entry, string, string) string     { return "stream" }
func (b *bytesBackend) UntrackStream(string)                                  {}

func (b *bytesBackend) OpenStream(
	ctx context.Context,
	entry *storage.Entry,
	filename string,
	offset int64,
	_ string,
) (manager.StreamReader, error) {
	return b.OpenStreamUntrackedForCache(ctx, entry, filename, offset)
}

func (b *bytesBackend) OpenStreamUntrackedForCache(
	_ context.Context,
	_ *storage.Entry,
	_ string,
	offset int64,
) (manager.StreamReader, error) {
	return &bytesStreamReader{Reader: bytes.NewReader(b.content[offset:])}, nil
}

type bytesStreamReader struct{ *bytes.Reader }

func (r *bytesStreamReader) Close() error { return nil }
func (r *bytesStreamReader) Prime() error { return nil }

// A handle opened while the cache is under budget keeps returning the file's
// bytes after the cache crosses its budget: reads fall back to streaming
// directly instead of failing with the cache writer's ENOSPC (EIO to FUSE).
func TestOpenHandleSurvivesCacheCrossingBudget(t *testing.T) {
	t.Parallel()
	const (
		entryName = "Movie"
		filename  = "movie.mkv"
		fileSize  = int64(4 << 20)
	)
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i*31 + 7)
	}
	backend := &bytesBackend{
		entry: &storage.Entry{
			Name:  entryName,
			Files: map[string]*storage.File{filename: {Name: filename, Size: fileSize}},
		},
		content: content,
	}
	cfg := dfsconfig.DefaultFuseConfig()
	cfg.CacheDir = t.TempDir()
	cfg.CacheCleanupInterval = time.Hour
	cfg.CacheDiskSize = 1 << 30
	cache, err := NewCache(context.Background(), backend, cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	cache.logger = zerolog.Nop()
	t.Cleanup(func() { _ = cache.Close() })

	item, err := cache.GetItem(entryName, filename, fileSize)
	if err != nil {
		t.Fatal(err)
	}
	file := NewStreamingFile(item)
	if file == nil {
		t.Fatal("NewStreamingFile returned nil")
	}
	t.Cleanup(func() { _ = file.Close() })

	// Opened under budget; other streams then fill the cache past it.
	cache.totalSize.Store(cache.threshold)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, off := range []int64{1 << 20, 3 << 20} {
		got := make([]byte, 256<<10)
		n, readErr := file.ReadAtContext(ctx, got, off)
		if readErr != nil {
			t.Fatalf("read at %d after crossing the budget: %v", off, readErr)
		}
		if !bytes.Equal(got[:n], content[off:off+int64(len(got))]) || n != len(got) {
			t.Fatalf("read at %d returned %d wrong bytes", off, n)
		}
	}
}
