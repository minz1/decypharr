//go:build !windows

package rclone

import (
	"errors"
	"os/exec"
	"syscall"
)

// WasHardTerminated reports true iff the process was ended by SIGKILL or SIGTERM.
func WasHardTerminated(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false
	}
	sig := ws.Signal()
	return sig == syscall.SIGKILL || sig == syscall.SIGTERM
}
