//go:build unix

package rclone

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup puts rcd in its own process group, so a signal sent to
// decypharr's group (a terminal ^C) does not reach it: decypharr unmounts
// first and then stops rcd itself.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
