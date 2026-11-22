//go:build !unix

package main

import "os/exec"

// detachAttrs: no session to leave on non-Unix systems; the server still
// runs on after the launcher exits.
func detachAttrs(cmd *exec.Cmd) {}
