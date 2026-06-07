//go:build !unix

package main

import "os/exec"

// setupProcessGroup is a no-op on platforms without POSIX process groups. The
// default CommandContext cancel (kill the direct child) plus cmd.WaitDelay still
// bound how long cmd.Run() can block after the deadline.
func setupProcessGroup(cmd *exec.Cmd) {}
