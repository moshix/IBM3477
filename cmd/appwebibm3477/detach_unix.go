//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detachAttrs runs the server in its own session: Ctrl-C in the shell and
// closing the terminal window (SIGHUP) no longer reach it.
func detachAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
