//go:build linux

package rclone

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const mountinfoChildEnv = "DECYPHARR_MOUNTINFO_CHILD"

func TestWatchMountTableStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- watchMountTable(ctx, func() {}) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchMountTable after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchMountTable did not return within 5s of cancel")
	}
}

func TestWatchMountTableCancelledBeforeStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := watchMountTable(ctx, func() {}); err != nil {
		t.Fatalf("watchMountTable on a done ctx = %v, want nil", err)
	}
}

func TestReadMountTableMissing(t *testing.T) {
	t.Parallel()
	entries, err := readMountTable(filepath.Join(t.TempDir(), "not-mounted"))
	if err != nil {
		t.Fatalf("readMountTable: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("readMountTable(unmounted) = %v, want none", entries)
	}
}

// TestWatchMountTableSeesAMount re-runs itself in a new user and mount namespace, where it may mount a tmpfs, and
// checks the watcher reports the change.
func TestWatchMountTableSeesAMount(t *testing.T) {
	if os.Getenv(mountinfoChildEnv) == "1" {
		watchChild(t)
		return
	}
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWatchMountTableSeesAMount$", "-test.v")
	cmd.Env = append(os.Environ(), mountinfoChildEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSPC) {
			t.Skipf("user namespaces unavailable: %v", err)
		}
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func watchChild(t *testing.T) {
	t.Helper()
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatalf("make / private: %v", err)
	}
	target := t.TempDir()
	w, err := openMountTable()
	if err != nil {
		t.Fatalf("openMountTable: %v", err)
	}
	defer w.close()
	// The watch is open, so this one mount is a change it must report.
	if err = unix.Mount("tmpfs", target, "tmpfs", 0, ""); err != nil {
		t.Fatalf("mount tmpfs: %v", err)
	}
	defer func() { _ = unix.Unmount(target, 0) }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	changed := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- w.run(ctx, func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		})
	}()
	select {
	case <-changed:
		cancel()
		if err = <-done; err != nil {
			t.Fatalf("run: %v", err)
		}
	case err = <-done:
		t.Fatalf("run returned early: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("no mount table change seen within 10s")
	}
}
