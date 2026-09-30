package vfs

import (
	"errors"
	"io/fs"
	"testing"
)

func TestStreamingFileCloseIsIdempotent(t *testing.T) {
	item := &CacheItem{info: ItemInfo{Size: 1 << 20}}
	file := NewStreamingFile(item)
	if file == nil {
		t.Fatal("NewStreamingFile returned nil")
	}
	if item.opens.Load() != 1 {
		t.Fatalf("open references=%d, want 1", item.opens.Load())
	}

	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if item.opens.Load() != 0 {
		t.Fatalf("open references=%d after close, want 0", item.opens.Load())
	}
	if _, err := file.ReadAt(make([]byte, 1), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("read after close error=%v, want fs.ErrClosed", err)
	}
}

func TestStreamingFileRejectsNegativeOffset(t *testing.T) {
	item := &CacheItem{info: ItemInfo{Size: 1 << 20}}
	file := NewStreamingFile(item)
	if file == nil {
		t.Fatal("NewStreamingFile returned nil")
	}
	t.Cleanup(func() { _ = file.Close() })

	if _, err := file.ReadAt(make([]byte, 1), -1); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("negative read error=%v, want fs.ErrInvalid", err)
	}
}

// Open-file stats are kept per cache item, so closing a handle that never
// touched the item (a direct stream for the same path) cannot knock the item's
// real handles out of the count, as the old name-keyed ReleaseFile did.
func TestActiveFilesTracksItemHandles(t *testing.T) {
	c := &Cache{}
	item := &CacheItem{cache: c, info: ItemInfo{Size: 1 << 20}}

	a, b := NewStreamingFile(item), NewStreamingFile(item)
	if a == nil || b == nil {
		t.Fatal("NewStreamingFile returned nil")
	}
	direct := &DirectStreamFile{size: 1 << 20}
	_ = direct.Close()

	if got := c.activeFiles.Load(); got != 1 {
		t.Fatalf("active files = %d with two handles open, want 1", got)
	}
	_ = a.Close()
	if got := c.activeFiles.Load(); got != 1 {
		t.Fatalf("active files = %d with one handle open, want 1", got)
	}
	_ = b.Close()
	if got := c.activeFiles.Load(); got != 0 {
		t.Fatalf("active files = %d after all handles closed, want 0", got)
	}
	if got := c.totalFiles.Load(); got != 1 {
		t.Fatalf("total files = %d, want 1", got)
	}
}
