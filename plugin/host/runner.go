package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/hashicorp/go-plugin/runner"
)

// procRunner is the go-plugin runner.Runner the host starts plugins with,
// in place of go-plugin's own command runner, which hands the plugin
// tobira's stdin. Here stdin is the null device, so a plugin never sees the
// TUI's keystrokes or `tobira mcp`'s JSON-RPC stream. On Linux the process
// also gets a parent-death signal (see start_linux.go).
type procRunner struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	pid    int
	// exited is closed once Wait returns; the Linux start keeps its OS
	// thread (which the parent-death signal is tied to) until then.
	exited chan struct{}
	runner.AddrTranslator
}

var _ runner.Runner = (*procRunner)(nil)

// newProcRunner builds the runner for cmd. go-plugin's spec, gp, carries
// the variables go-plugin adds (the cookie, PLUGIN_*); they are appended to
// cmd's allow-listed environment.
func newProcRunner(cmd *exec.Cmd, gp *exec.Cmd) (*procRunner, error) {
	cmd.Env = append(cmd.Env, gp.Env...)
	cmd.Stdin = nil // os/exec connects the null device
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	return &procRunner{cmd: cmd, stdout: stdout, stderr: stderr, exited: make(chan struct{}), AddrTranslator: identity{}}, nil
}

// Start implements runner.Runner.
func (r *procRunner) Start(context.Context) error {
	if err := startProcess(r.cmd, r.exited); err != nil {
		return err
	}
	r.pid = r.cmd.Process.Pid
	return nil
}

// Wait implements runner.Runner.
func (r *procRunner) Wait(context.Context) error {
	defer close(r.exited)
	return r.cmd.Wait()
}

// Kill implements runner.Runner.
func (r *procRunner) Kill(context.Context) error {
	if r.cmd.Process == nil {
		return nil
	}
	if err := r.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (r *procRunner) Stdout() io.ReadCloser { return r.stdout }
func (r *procRunner) Stderr() io.ReadCloser { return r.stderr }
func (r *procRunner) Name() string          { return r.cmd.Path }
func (r *procRunner) ID() string            { return strconv.Itoa(r.pid) }

// Diagnose implements runner.Runner.
func (r *procRunner) Diagnose(context.Context) string {
	return fmt.Sprintf("the plugin at %s did not complete the tobira plugin handshake "+
		"(wrong architecture, missing libraries, or not a tobira driver plugin)", r.cmd.Path)
}

// identity translates no addresses: the plugin runs on this host.
type identity struct{}

func (identity) PluginToHost(n, a string) (string, string, error) { return n, a, nil }
func (identity) HostToPlugin(n, a string) (string, string, error) { return n, a, nil }
