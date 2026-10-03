package fsutil_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sirrobot01/decypharr/internal/fsutil"
)

func TestMkdirSharedSetsSetgidOnCreatedDirs(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no setgid on windows")
	}
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(existing, "a", "b")
	if err := fsutil.MkdirShared(leaf, 0o770|fs.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(existing, "a"), leaf} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&fs.ModeSetgid == 0 || info.Mode().Perm()&0o007 != 0 {
			t.Errorf("%s mode = %v, want setgid and no access for others", dir, info.Mode())
		}
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSetgid != 0 || info.Mode().Perm() != 0o700 {
		t.Errorf("pre-existing dir changed to %v", info.Mode())
	}
}

func TestMkdirSharedWithoutSetgid(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "plain")
	if err := fsutil.MkdirShared(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSetgid != 0 {
		t.Fatalf("mode = %v", info.Mode())
	}
	// Idempotent on an existing directory.
	if err = fsutil.MkdirShared(dir, 0o750); err != nil {
		t.Fatal(err)
	}
}

func TestJoinNameKeepsNamesInsideDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("srv", "media")
	for _, name := range []string{"Movie (2023).mkv", "a..b", ".hidden", "name with spaces"} {
		got, err := fsutil.JoinName(dir, name)
		if err != nil || got != filepath.Join(dir, name) {
			t.Errorf("JoinName(%q) = %q, %v", name, got, err)
		}
	}
	unsafe := []string{"", ".", "..", "../escape", "sub/file", "/abs", "nul\x00byte"}
	if runtime.GOOS == "windows" {
		unsafe = append(unsafe, `sub\file`, "CON")
	}
	for _, name := range unsafe {
		if got, err := fsutil.JoinName(dir, name); !errors.Is(err, fsutil.ErrUnsafeName) {
			t.Errorf("JoinName(%q) = %q, %v; want ErrUnsafeName", name, got, err)
		}
	}
}
