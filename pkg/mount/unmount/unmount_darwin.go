package unmount

import (
	"errors"

	"golang.org/x/sys/unix"
)

// unmountSyscall detaches path with unmount(2): normally, then forced
// (MNT_FORCE) when the mount is busy or its daemon is gone.
func unmountSyscall(path string) error {
	err := unix.Unmount(path, 0)
	if err == nil {
		return nil
	}
	forceErr := unix.Unmount(path, unix.MNT_FORCE)
	if forceErr == nil {
		return nil
	}
	return errors.Join(err, forceErr)
}
