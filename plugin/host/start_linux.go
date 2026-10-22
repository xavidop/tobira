//go:build linux

package host

import (
	"os/exec"
	"runtime"
	"syscall"
)

// startProcess starts cmd with SIGKILL as its parent-death signal, so a
// plugin dies with tobira even when tobira is killed outright and the
// plugin is not built with the SDK (which watches its parent itself). The
// signal fires when the thread that started the process exits, so the
// start runs on a goroutine locked to its OS thread until the process has
// exited.
func startProcess(cmd *exec.Cmd, exited <-chan struct{}) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			errc <- err
			return
		}
		errc <- nil
		<-exited
	}()
	return <-errc
}
