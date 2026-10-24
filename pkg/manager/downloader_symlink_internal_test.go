package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func symlinkTestDownloader(t *testing.T) *Downloader {
	t.Helper()
	return &Downloader{
		logger:           zerolog.Nop(),
		manager:          withTestConfig(t, &Manager{}),
		mountWaitTimeout: 5 * time.Second,
	}
}

func TestCreateSymlinksLinksFileThatAppearsLater(t *testing.T) {
	t.Parallel()
	mountPath := t.TempDir()
	symlinkDir := t.TempDir()
	target := filepath.Join(mountPath, "release.mkv")
	timer := time.AfterFunc(300*time.Millisecond, func() {
		_ = os.WriteFile(target, []byte("media"), 0o644)
	})
	defer timer.Stop()

	paths, err := symlinkTestDownloader(t).createSymlinksWhenMountFilesAppear(
		t.Context(), &storage.Entry{Name: "release"}, []*storage.File{{Name: "release.mkv"}}, mountPath, symlinkDir)
	if err != nil {
		t.Fatalf("create symlinks: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("created %d symlinks, want 1", len(paths))
	}
	got, err := os.Readlink(paths[0])
	if err != nil || got != target {
		t.Fatalf("symlink target = %q (err %v), want %q", got, err, target)
	}
}

func TestCreateSymlinksTimesOutWhenFileNeverAppears(t *testing.T) {
	t.Parallel()
	d := symlinkTestDownloader(t)
	d.mountWaitTimeout = 300 * time.Millisecond
	_, err := d.createSymlinksWhenMountFilesAppear(
		t.Context(), &storage.Entry{Name: "release"}, []*storage.File{{Name: "release.mkv"}}, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "timeout waiting for mount files") {
		t.Fatalf("err = %v, want mount-files timeout", err)
	}
}

func TestCreateSymlinksIgnoresDirectoryWithFileName(t *testing.T) {
	t.Parallel()
	mountPath := t.TempDir()
	symlinkDir := t.TempDir()
	nested := filepath.Join(mountPath, "release.mkv")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "release.mkv"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := symlinkTestDownloader(t)
	d.mountWaitTimeout = 300 * time.Millisecond
	paths, err := d.createSymlinksWhenMountFilesAppear(
		t.Context(), &storage.Entry{Name: "release"}, []*storage.File{{Name: "release.mkv"}}, mountPath, symlinkDir)
	if err == nil || !strings.Contains(err.Error(), "timeout waiting for mount files") {
		t.Fatalf("err = %v, paths = %v, want mount-files timeout", err, paths)
	}
	if entries, _ := os.ReadDir(symlinkDir); len(entries) != 0 {
		t.Fatalf("symlinkDir has %d entries, want 0", len(entries))
	}
}

// A delete after the symlinks existed used to re-save the entry as completed and send the arr to import them.
func TestDeleteDuringSymlinkActionLeavesNoEntry(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Stop() })
	entry := &storage.Entry{
		InfoHash:       "0123456789012345678901234567890123456789",
		Name:           "Show.S01E01",
		Protocol:       config.ProtocolTorrent,
		State:          storage.EntryStateDownloading,
		ActiveProvider: "provider",
		Action:         config.DownloadActionSymlink,
		SavePath:       t.TempDir(),
		Files:          map[string]*storage.File{"Show.S01E01.mkv": {Name: "Show.S01E01.mkv", Size: 5}},
		Providers:      map[string]*storage.ProviderEntry{},
	}
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	mountPath := m.GetTorrentMountPath(entry)
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountPath, "Show.S01E01.mkv"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.queue.Delete(entry.InfoHash, false, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errEntryDeleted)

	if err := m.downloader.processSymlink(ctx, entry, mountPath); !errors.Is(err, context.Canceled) {
		t.Errorf("processSymlink err = %v, want context.Canceled", err)
	}
	if saved, err := m.queue.GetTorrent(entry.InfoHash); err == nil {
		t.Fatalf("deleted entry re-saved: complete=%v downloading=%v", saved.IsComplete, saved.IsDownloading)
	}
}

// A delete mid warm used to leave the warm reading the mount for up to CacheWarmTimeout.
func TestWarmFileCacheStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	m := &Manager{logger: zerolog.New(&logs)}
	path := filepath.Join(t.TempDir(), "release.mkv")
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errEntryDeleted)

	if err := m.WarmFileCache(ctx, []string{path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), context.Canceled.Error()) {
		t.Fatalf("warm ran on a cancelled action: logs %q", logs.String())
	}
}

// A delete before the NZB download began used to re-save the entry as downloading, an orphan with no worker.
func TestDeleteBeforeUsenetDownloadLeavesNoEntry(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	t.Cleanup(func() { _ = m.Stop() })
	m.usenet = newUnreachableUsenet(t)
	entry := &storage.Entry{
		InfoHash: "0123456789012345678901234567890123456789",
		Name:     "Show.S01E01",
		Protocol: config.ProtocolNZB,
		State:    storage.EntryStateDownloading,
		Action:   config.DownloadActionDownload,
		SavePath: t.TempDir(),
		Files:    map[string]*storage.File{"Show.S01E01.mkv": {Name: "Show.S01E01.mkv", Size: 5}},
	}
	if err := m.queue.Add(entry); err != nil {
		t.Fatal(err)
	}
	if err := m.queue.Delete(entry.InfoHash, false, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errEntryDeleted)

	if err := m.downloader.processUsenetDownload(ctx, entry); err == nil {
		t.Error("processUsenetDownload err = nil, want the cancellation")
	}
	if saved, err := m.queue.GetTorrent(entry.InfoHash); err == nil {
		t.Fatalf("deleted entry re-saved: downloading=%v", saved.IsDownloading)
	}
}
