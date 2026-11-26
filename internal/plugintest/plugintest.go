// Package plugintest builds the plugin host's test plugin
// (plugin/host/testdata/plugin) for the tests that run a real plugin
// process. Nothing here is for production.
package plugintest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Build compiles the test plugin to out. repoRoot is the repository root,
// absolute or relative to the working directory.
func Build(repoRoot, out string) error {
	return build(
		filepath.Join(repoRoot, "plugin", "host", "testdata", "plugin"),
		[]string{
			filepath.Join(repoRoot, "go.sum"),
			filepath.Join(repoRoot, "plugin", "sdk", "go.sum"),
		},
		out,
	)
}

// build compiles the module in dir to out.
//
// The module replaces the root and plugin/sdk modules with the working
// tree, so they decide the versions of the dependencies it shares with
// them, and a dependency bump there leaves the indirect requirements in
// dir/go.mod behind. A plain build then stops at "updates to go.mod
// needed". To build what the working tree asks for, the go command reads a
// temporary copy of go.mod that it may bring up to date (-mod=mod); the
// files in dir are not written.
//
// sums are the go.sum files of the replaced modules. Every version the
// copy can move to is one they require, so their checksums are added to
// the copy's go.sum and the build needs no network beyond a plain one.
func build(dir string, sums []string, out string) error {
	out, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "tobira-plugintest")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return err
	}
	modfile := filepath.Join(tmp, "go.mod")
	if err := os.WriteFile(modfile, mod, 0o600); err != nil {
		return err
	}
	var sum bytes.Buffer
	for _, name := range append([]string{filepath.Join(dir, "go.sum")}, sums...) {
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		sum.Write(b)
		if len(b) > 0 && b[len(b)-1] != '\n' {
			sum.WriteByte('\n')
		}
	}
	// The go command pairs a -modfile with the .sum file beside it.
	if err := os.WriteFile(filepath.Join(tmp, "go.sum"), sum.Bytes(), 0o600); err != nil {
		return err
	}

	cmd := exec.Command("go", "build", "-mod=mod", "-modfile="+modfile, "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, b)
	}
	return nil
}
