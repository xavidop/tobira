package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/examples/custom-driver-plugin/memdemo"
	"github.com/xavidop/tobira/plugin/host"
)

// fixture is the conformance input for memdemo, the same in process and
// through the plugin host.
var fixture = drivertest.Fixture{
	Namespace:        memdemo.Namespace,
	Collection:       drivertest.PeopleCollection(),
	SelectAll:        driver.Query{Text: "select * from tobira_people"},
	Missing:          driver.Query{Text: "select * from nope"},
	Write:            driver.Query{Text: "delete from tobira_people"},
	Slow:             driver.Query{Text: "sleep 10s"},
	ParamQuery:       driver.Query{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(7)}},
	TxWrite:          driver.Query{Text: "delete from tobira_people where id = 2"},
	TxRestore:        driver.Query{Text: "reset tobira_people"},
	CompleteAt:       driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
	AutoIncrementKey: drivertest.Bool(false),
}

// conformance runs drivertest Strict against d, opening every Conn with
// d.Open.
func conformance(t *testing.T, d driver.Driver) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := d.Open(context.Background(), driver.ConnConfig{Name: "demo", DSN: "memdemo://", ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       d,
		Open:         open(false),
		OpenReadOnly: open(true),
		Strict:       true,
		Fixture:      fixture,
	})
}

// TestConformanceInProcess is the reference: the driver as a Go value.
func TestConformanceInProcess(t *testing.T) {
	conformance(t, memdemo.New())
}

// TestConformanceThroughTheHost builds this module's plugin binary,
// discovers and registers it the way tobira does at startup, and runs the
// same Strict suite against the registry's driver: every call crosses the
// plugin protocol into a separate process.
func TestConformanceThroughTheHost(t *testing.T) {
	// Snapshot before the host exists: after Host.Close, nothing it
	// started may be left running.
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		if err := goleak.Find(before); err != nil {
			t.Errorf("goroutines left after Host.Close: %v", err)
		}
	})

	dir := buildPlugin(t)
	plugins := host.Discover([]string{dir})
	if len(plugins) != 1 || plugins[0].Name != memdemo.Name {
		t.Fatalf("Discover(%s) = %+v, want the memdemo plugin", dir, plugins)
	}
	reg := driver.NewRegistry()
	h := host.Register(reg, plugins, host.Options{})
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Host.Close: %v", err)
		}
	})

	// Register starts nothing and Info() answers a stub until the plugin
	// has answered once. Host.Info starts the plugin and waits for its
	// Info, so Strict checks the real one.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := h.Info(ctx, memdemo.Name)
	if err != nil {
		t.Fatalf("Host.Info: %v", err)
	}
	if want := memdemo.New().Info(); !sameInfo(info, want) {
		t.Fatalf("Info through the host = %+v, want the driver's %+v", info, want)
	}
	d, ok := reg.Lookup(memdemo.Name)
	if !ok {
		t.Fatal("the registry has no memdemo driver")
	}
	if p, ok := host.PluginOf(d); !ok || p.Name != memdemo.Name {
		t.Fatalf("PluginOf(memdemo) = %+v, %v: want the plugin", p, ok)
	}
	if got := d.Info(); !sameInfo(got, info) {
		t.Fatalf("registry Info() = %+v after Host.Info, want the cached %+v", got, info)
	}
	conformance(t, d)
}

func sameInfo(a, b driver.Info) bool {
	return a.Name == b.Name && a.Kind == b.Kind && a.Dialect == b.Dialect && a.Version == b.Version &&
		slices.Equal(a.Schemes, b.Schemes) && slices.Equal(a.Capabilities, b.Capabilities)
}

// pluginFile is the binary name tobira discovers: tobira-driver-<name>.
func pluginFile() string {
	name := host.Prefix + memdemo.Name
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// buildPlugin builds this module's main package into a fresh directory as
// tobira-driver-memdemo, mode 0755, and returns the directory. The
// directory is the test's own (0700), so the host's permission checks
// pass: neither the binary nor its directory is world-writable.
func buildPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, pluginFile())
	goBuild(t, ".", bin)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// goBuild builds the main package in pkgDir into out, the way make does
// (no workspace).
func goBuild(t *testing.T, pkgDir, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkgDir, err, b)
	}
}
