package rclone

import (
	"fmt"
	"os"
	"os/exec"
)

// execProcess is a started rcd command as the owner sees it.
type execProcess struct {
	cmd *exec.Cmd
}

// Wait blocks until the process exits. The reaper is its only caller.
func (p *execProcess) Wait() error {
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("rcd exited: %w", err)
	}
	return nil
}

func (p *execProcess) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

func (p *execProcess) Kill() error { return p.cmd.Process.Kill() }

func (p *execProcess) Pid() int { return p.cmd.Process.Pid }
