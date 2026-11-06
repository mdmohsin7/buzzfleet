//go:build !linux

package main

import "os/exec"

// dieWithParent does nothing where there is no parent-death signal: the
// coding agent then exits when it next reads EOF on its stdin, as it does
// once the gate is gone.
func dieWithParent(*exec.Cmd) {}
