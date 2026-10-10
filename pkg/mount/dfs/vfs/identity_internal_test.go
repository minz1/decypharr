package vfs

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	dfsconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A release grabbed again under the same name and size is a different entry
// with different bytes. Its reads must not be served from the cache of the
// entry it replaced, neither from the live item nor, after a restart, from
// the ranges persisted on disk (a stale cache served a garbage head for a
// verified NZB).
func TestCacheDoesNotServeAReplacedEntrysBytes(t *testing.T) {
	t.Parallel()
	const (
		name     = "Show.S01E01"
		filename = "show.mkv"
		size     = int64(1 << 20)
	)
	entry := func(hash string, fill byte) (*storage.Entry, []byte) {
		return &storage.Entry{
			InfoHash: hash,
			Name:     name,
			Files:    map[string]*storage.File{filename: {Name: filename, Size: size}},
		}, bytes.Repeat([]byte{fill}, int(size))
	}
	oldEntry, oldBytes := entry("old", 0xAA)
	newEntry, newBytes := entry("new", 0xBB)
	backend := &bytesBackend{entry: oldEntry, content: oldBytes}

	cfg := dfsconfig.DefaultFuseConfig()
	cfg.CacheDir = t.TempDir()
	cfg.CacheCleanupInterval = time.Hour
	cfg.CacheDiskSize = 1 << 30
	newCache := func() *Cache {
		cache, err := NewCache(context.Background(), backend, cfg, zerolog.Nop())
		if err != nil {
			t.Fatal(err)
		}
		cache.logger = zerolog.Nop()
		return cache
	}
	head := func(cache *Cache) []byte {
		t.Helper()
		item, err := cache.GetItem(backend.entry, filename, size)
		if err != nil {
			t.Fatal(err)
		}
		file := NewStreamingFile(item)
		if file == nil {
			t.Fatal("NewStreamingFile returned nil")
		}
		defer func() { _ = file.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		got := make([]byte, 64<<10)
		if _, readErr := file.ReadAtContext(ctx, got, 0); readErr != nil {
			t.Fatal(readErr)
		}
		return got
	}

	cache := newCache()
	if got := head(cache); !bytes.Equal(got, oldBytes[:len(got)]) {
		t.Fatal("old entry read wrong bytes")
	}
	backend.entry, backend.content = newEntry, newBytes
	if got := head(cache); !bytes.Equal(got, newBytes[:len(got)]) {
		t.Errorf("live cache served the replaced entry's bytes: % x", got[:4])
	}
	_ = cache.Close()

	backend.entry, backend.content = oldEntry, oldBytes
	restarted := newCache()
	_ = head(restarted)
	_ = restarted.Close()
	backend.entry, backend.content = newEntry, newBytes
	restarted = newCache()
	t.Cleanup(func() { _ = restarted.Close() })
	if got := head(restarted); !bytes.Equal(got, newBytes[:len(got)]) {
		t.Errorf("restarted cache served the replaced entry's persisted bytes: % x", got[:4])
	}
}
