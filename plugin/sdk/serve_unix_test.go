//go:build unix

package sdk

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestPluginShutsDownWhenItsHostDies kills the host process with SIGKILL,
// so no Shutdown RPC is ever sent: the plugin must notice it was orphaned,
// close its Conns and exit on its own.
func TestPluginShutsDownWhenItsHostDies(t *testing.T) {
	if testing.Short() {
		t.Skip("launches subprocesses")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "closed")
	host := exec.Command(exe, "-test.run=^$")
	host.Env = []string{parentEnv + "=" + marker, "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}
	out, err := host.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = host.Process.Kill()
		t.Fatalf("host never reported the plugin pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("pid line %q", line)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if err := host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = host.Wait()

	deadline := time.Now().Add(15 * time.Second)
	for {
		_, statErr := os.Stat(marker)
		gone := errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		if statErr == nil && gone {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphaned plugin: conn closed=%v, process gone=%v", statErr == nil, gone)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
