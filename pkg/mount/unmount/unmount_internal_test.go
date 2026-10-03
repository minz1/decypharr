package unmount

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rs/zerolog"
)

func TestCleanMountPath(t *testing.T) {
	t.Parallel()
	for path, ok := range map[string]bool{
		"/mnt/decypharr":     true,
		"/mnt/decypharr/../": true,
		"":                   false,
		"  ":                 false,
		"relative/mount":     false,
		"/":                  false,
		"/mnt/a\x00b":        false,
	} {
		if _, err := cleanMountPath(path); (err == nil) != ok {
			t.Errorf("cleanMountPath(%q) err = %v, want ok=%v", path, err, ok)
		}
	}
}

// New only ever keeps absolute helper paths.
func TestNewResolvesAbsoluteHelpers(t *testing.T) {
	t.Parallel()
	for _, helper := range New(zerolog.Nop()).helpers {
		if !filepath.IsAbs(helper) {
			t.Fatalf("helper %q is not absolute", helper)
		}
	}
}

// A path that is not a mount fails every attempt and reports them all.
func TestUnmountReportsFailureForPlainDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no unmount syscall on windows")
	}
	u := &Unmounter{log: zerolog.Nop()}
	if err := u.Unmount(context.Background(), t.TempDir()); err == nil {
		t.Fatal("unmounting a plain directory succeeded")
	}
	if err := u.Unmount(context.Background(), ""); err == nil {
		t.Fatal("unmounting an empty path succeeded")
	}
}

// A helper that succeeds ends the chain.
func TestUnmountFallsBackToHelper(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell script helper")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	helper := filepath.Join(dir, "fusermount")
	script := "#!/bin/sh\n[ \"$1 $2 $3\" = \"-u -z --\" ] && echo \"$4\" > " + marker + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	u := &Unmounter{helpers: []string{helper}, log: zerolog.Nop()}
	mountPath := t.TempDir()
	if err := u.Unmount(context.Background(), mountPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != mountPath+"\n" {
		t.Fatalf("helper saw %q, %v; want %q", got, err, mountPath)
	}
}
