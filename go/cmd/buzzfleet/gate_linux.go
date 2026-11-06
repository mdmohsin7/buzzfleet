package main

import (
	"os/exec"
	"runtime"
	"syscall"
)

// dieWithParent has the kernel kill the coding agent when the gate dies.
// Pdeathsig follows the thread that started the child, not the process, so
// that thread is kept for the gate's whole life.
func dieWithParent(cmd *exec.Cmd) {
	runtime.LockOSThread()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
