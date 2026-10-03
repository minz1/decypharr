//go:build !linux && !darwin

package unmount

import (
	"fmt"
	"runtime"
)

// unmountSyscall has no direct equivalent here; the FUSE helpers, if any,
// are tried next.
func unmountSyscall(string) error {
	return fmt.Errorf("unmount syscall not supported on %s", runtime.GOOS)
}
