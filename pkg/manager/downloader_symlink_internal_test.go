package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

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
