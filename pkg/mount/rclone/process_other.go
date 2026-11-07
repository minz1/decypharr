//go:build !unix

package rclone

import "os/exec"

// ownProcessGroup does nothing where there are no Unix process groups.
func ownProcessGroup(_ *exec.Cmd) {}
