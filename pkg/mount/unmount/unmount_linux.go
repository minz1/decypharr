package unmount

import (
	"errors"

	"golang.org/x/sys/unix"
)

// unmountSyscall detaches path with umount2: normally, then lazily
// (MNT_DETACH) when the mount is busy or its daemon is gone.
func unmountSyscall(path string) error {
	err := unix.Unmount(path, 0)
	if err == nil {
		return nil
	}
	lazyErr := unix.Unmount(path, unix.MNT_DETACH)
	if lazyErr == nil {
		return nil
	}
	return errors.Join(err, lazyErr)
}
