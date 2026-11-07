//go:build unix

package rclone

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// rcd leads its own process group, so a signal to decypharr's group skips it.
func TestRcdLeadsItsOwnProcessGroup(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRcd$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_RCD=1")
	ownProcessGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if startErr := cmd.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	proc := &execProcess{cmd: cmd}
	if line, readErr := bufio.NewReader(out).ReadString('\n'); readErr != nil || !strings.HasPrefix(line, "ready") {
		t.Fatalf("helper said %q, %v", line, readErr)
	}

	pgid, pgErr := syscall.Getpgid(proc.Pid())
	if sigErr := proc.Signal(os.Interrupt); sigErr != nil {
		t.Fatal(sigErr)
	}
	if waitErr := proc.Wait(); waitErr != nil {
		t.Fatalf("Wait = %v", waitErr)
	}
	if pgErr != nil {
		t.Fatal(pgErr)
	}
	if pgid != proc.Pid() {
		t.Fatalf("pgid = %d, want the rcd pid %d", pgid, proc.Pid())
	}
	if pgid == syscall.Getpgrp() {
		t.Fatalf("rcd shares decypharr's process group %d", pgid)
	}
}
