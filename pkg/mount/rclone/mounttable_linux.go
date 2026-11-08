//go:build linux

package rclone

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

const mountinfoPath = "/proc/self/mountinfo"

// watchMountTable calls changed after each change to this process's mount table, until ctx is done (then it returns
// nil) or the watch fails (then it returns the error). The kernel signals a change on /proc/self/mountinfo with
// POLLPRI; the poll itself rearms it, and changes between polls coalesce. An eventfd in the same poll set wakes it
// when ctx ends.
func watchMountTable(ctx context.Context, changed func()) error {
	table, err := unix.Open(mountinfoPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("%w: no %s", errNoMountTable, mountinfoPath)
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", mountinfoPath, err)
	}
	defer func() { _ = unix.Close(table) }()
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return fmt.Errorf("eventfd: %w", err)
	}
	defer func() { _ = unix.Close(wake) }()
	tableFd, okTable := pollFd(table)
	wakeFd, okWake := pollFd(wake)
	if !okTable || !okWake {
		return fmt.Errorf("poll %s: descriptor out of range", mountinfoPath)
	}

	// The write happens only while both fds are open: on return we either stop the AfterFunc before it runs or wait
	// for it to finish, and only then do the deferred closes run.
	wrote := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(wrote)
		var one [8]byte
		binary.NativeEndian.PutUint64(one[:], 1)
		_, _ = unix.Write(wake, one[:])
	})
	defer func() {
		if !stop() {
			<-wrote
		}
	}()

	fds := []unix.PollFd{
		{Fd: tableFd, Events: unix.POLLPRI},
		{Fd: wakeFd, Events: unix.POLLIN},
	}
	for {
		if _, err = unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("poll %s: %w", mountinfoPath, err)
		}
		if fds[1].Revents&unix.POLLIN != 0 {
			return nil
		}
		if fds[0].Revents&unix.POLLNVAL != 0 || fds[1].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
			return fmt.Errorf("poll %s: revents %#x/%#x", mountinfoPath, fds[0].Revents, fds[1].Revents)
		}
		if fds[0].Revents&(unix.POLLPRI|unix.POLLERR) != 0 {
			changed()
		}
	}
}

// readMountTable counts the FUSE entries at path in this process's mount table. It is a procfs read and never a
// FUSE round-trip, so it cannot hang on our own mount.
func readMountTable(path string) (int, error) {
	table, err := os.ReadFile(mountinfoPath)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%w: no %s", errNoMountTable, mountinfoPath)
	}
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", mountinfoPath, err)
	}
	return countFuseAt(table, path), nil
}

// pollFd is fd as a poll descriptor; false if it does not fit.
func pollFd(fd int) (int32, bool) {
	if fd < 0 || fd > math.MaxInt32 {
		return 0, false
	}
	return int32(fd), true
}
