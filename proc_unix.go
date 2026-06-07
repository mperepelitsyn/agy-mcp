//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup makes the child the leader of a new process group and, on
// context cancel/timeout, kills the ENTIRE group (negative PID) rather than just
// the direct child. This terminates grandchildren the child spawned (shells,
// node, MCP subprocesses under --dangerously-skip-permissions) which would
// otherwise survive holding the inherited stdout/stderr pipes and block
// cmd.Run() past the deadline. Paired with cmd.WaitDelay as a backstop.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID signals the whole process group led by the child.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// Fall back to killing just the child if the group kill fails
			// (e.g. the child already exited and reaped its group).
			return cmd.Process.Kill()
		}
		return nil
	}
}
