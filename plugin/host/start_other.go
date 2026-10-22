//go:build !linux

package host

import "os/exec"

// startProcess starts cmd. Only Linux has a parent-death signal; elsewhere
// SDK plugins watch their parent themselves.
func startProcess(cmd *exec.Cmd, _ <-chan struct{}) error { return cmd.Start() }
