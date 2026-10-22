package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/plugin/host"
)

// fakePlugin writes an executable named like a plugin. It is never
// started: registering a plugin does not launch it.
func fakePlugin(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tobira-driver-"+name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// startup runs `tobira version` with the given config text and
// environment, and returns the registry it ran with.
func startup(t *testing.T, config string, env map[string]string) *driver.Registry {
	t.Helper()
	reg := driver.NewRegistry()
	if err := reg.Add(drivertest.NewFake().Driver()); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	root := cli.NewRoot(cli.BuildInfo{Version: "test"}, cli.Streams{Out: &out, Err: &errOut}, cli.Deps{
		Registry: reg,
		Getenv:   func(k string) string { return env[k] },
		Home:     t.TempDir(),
		Environ:  func() []string { return nil },
	})
	if code := cli.Execute(context.Background(), root, []string{"--config", cfg, "version"}); code != 0 {
		t.Fatalf("version: code %d, stderr %q", code, errOut.String())
	}
	return reg
}

func isPlugin(reg *driver.Registry, name string) bool {
	d, ok := reg.Lookup(name)
	if !ok {
		return false
	}
	_, ok = host.PluginOf(d)
	return ok
}

func TestPluginsAreRegisteredAtStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits are unix semantics")
	}
	xdg, pathDir := t.TempDir(), t.TempDir()
	fakePlugin(t, filepath.Join(xdg, "tobira", "plugins"), "ext")
	fakePlugin(t, pathDir, "onpath")
	fakePlugin(t, pathDir, "fake") // the built-in wins
	reg := startup(t, "connections: []\n", map[string]string{"XDG_CONFIG_HOME": xdg, "PATH": pathDir})

	for _, name := range []string{"ext", "onpath"} {
		if !isPlugin(reg, name) {
			t.Errorf("plugin %q not registered", name)
		}
	}
	if isPlugin(reg, "fake") {
		t.Error("a plugin replaced the built-in fake driver")
	}
}

func TestPluginsCanBeDisabled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits are unix semantics")
	}
	xdg, pathDir := t.TempDir(), t.TempDir()
	fakePlugin(t, filepath.Join(xdg, "tobira", "plugins"), "ext")
	fakePlugin(t, pathDir, "onpath")
	reg := startup(t, "plugins:\n  enabled: false\n", map[string]string{"XDG_CONFIG_HOME": xdg, "PATH": pathDir})
	if isPlugin(reg, "ext") || isPlugin(reg, "onpath") {
		t.Error("plugins registered with plugins.enabled: false")
	}
}

func TestPluginsDirsOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits are unix semantics")
	}
	xdg, custom := t.TempDir(), t.TempDir()
	fakePlugin(t, filepath.Join(xdg, "tobira", "plugins"), "default-dir")
	fakePlugin(t, custom, "custom-dir")
	reg := startup(t, "plugins:\n  dirs: ["+custom+"]\n", map[string]string{"XDG_CONFIG_HOME": xdg})
	if !isPlugin(reg, "custom-dir") {
		t.Error("plugin in plugins.dirs not registered")
	}
	if isPlugin(reg, "default-dir") {
		t.Error("plugins.dirs should replace the default plugins directory")
	}
}
