package host_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/plugin/convert"
	"github.com/xavidop/tobira/plugin/host"
)

func TestPluginStdinIsNull(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if _, err := io.WriteString(w, "keystrokes"); err != nil {
		t.Fatal(err)
	}
	// The writer stays open: a plugin sharing tobira's stdin would read
	// the keystrokes and then block.
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()

	f := setup(t, baseEnv(""))
	conn := open(t, f, driver.ConnConfig{})
	rows, err := collect(ctxT(t, 20*time.Second), conn, "!stdin")
	if err != nil || len(rows) != 1 {
		t.Fatalf("!stdin = %v, %v", rows, err)
	}
	if got, _ := rows[0][0].AsText(); got != "eof:" {
		t.Errorf("the plugin's stdin gave %q, want an immediate EOF (/dev/null)", got)
	}
}

func TestDriversNeverBlocksOnAPlugin(t *testing.T) {
	f := setup(t, baseEnv("stall"))
	for i := range 2 {
		start := time.Now()
		infos := f.reg.Drivers()
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("Drivers() call %d took %s with a stalled plugin", i, d)
		}
		if len(infos) != 1 || infos[0].Name != pluginName || len(infos[0].Capabilities) != 0 {
			t.Errorf("Drivers() = %+v, want the stub for %s", infos, pluginName)
		}
	}
}

func TestSessionStateNeverBlocksOnAPlugin(t *testing.T) {
	f := setup(t, baseEnv("stall"))
	cfg := &config.Config{Connections: []config.Connection{{Name: "p", Driver: pluginName}}}
	mgr := session.New(cfg, f.reg, secrets.Resolver{}, false)
	start := time.Now()
	st := mgr.State("p")
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("State took %s with a stalled plugin", d)
	}
	if st.Info.Name != pluginName {
		t.Errorf("State.Info = %+v, want the stub", st.Info)
	}
}

// startsFile is a file the test plugin appends a line to each time it
// starts (TOBIRA_PLUGIN_TEST_STARTS), and the environment that says so.
func startsFile(t *testing.T, mode string) (string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "starts")
	return path, append(baseEnv(mode), "TOBIRA_PLUGIN_TEST_STARTS="+path)
}

// starts is how many times the test plugin has started.
func starts(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestInfoNeverStartsAPlugin(t *testing.T) {
	path, env := startsFile(t, "")
	f := setup(t, env)
	cfg := &config.Config{Connections: []config.Connection{{Name: "p", Driver: pluginName}}}
	mgr := session.New(cfg, f.reg, secrets.Resolver{}, false)
	for range 3 {
		stub := f.drv.Info()
		if stub.Name != pluginName || stub.Kind != driver.KindOther || len(stub.Capabilities) != 0 {
			t.Errorf("Info() = %+v, want the stub", stub)
		}
		if _, ok := host.InfoIfStarted(f.drv); ok {
			t.Error("InfoIfStarted reports a real Info before the plugin answered")
		}
		_ = f.reg.Drivers()
		_ = mgr.Drivers()
		if st := mgr.State("p"); st.Info.Name != pluginName {
			t.Errorf("State.Info = %+v, want the stub", st.Info)
		}
	}
	// A background start would have written the file well within this.
	time.Sleep(time.Second)
	if n := starts(t, path); n != 0 {
		t.Fatalf("the stub Info started the plugin %d times", n)
	}

	want := drivertest.NewFake().Driver().Info().Version
	if _, err := f.host.Info(ctxT(t, 20*time.Second), pluginName); err != nil {
		t.Fatalf("Host.Info: %v", err)
	}
	if n := starts(t, path); n != 1 {
		t.Errorf("Host.Info started the plugin %d times, want 1", n)
	}
	if info := f.drv.Info(); info.Version != want {
		t.Errorf("Info() after Host.Info = %+v, want the plugin's", info)
	}
	if info, ok := host.InfoIfStarted(f.drv); !ok || info.Version != want {
		t.Errorf("InfoIfStarted = %+v, %v", info, ok)
	}
	if _, ok := host.InfoIfStarted(drivertest.NewFake().Driver()); ok {
		t.Error("InfoIfStarted reports a built-in driver as a plugin")
	}
}

func TestOpenLoadsInfo(t *testing.T) {
	path, env := startsFile(t, "")
	f := setup(t, env)
	conn := open(t, f, driver.ConnConfig{})
	if _, ok := driver.As[driver.Browser](conn); !ok {
		t.Error("the Conn lacks the plugin's browser capability")
	}
	want := drivertest.NewFake().Driver().Info()
	if info, ok := host.InfoIfStarted(f.drv); !ok || !slices.Equal(info.Capabilities, want.Capabilities) {
		t.Errorf("InfoIfStarted after Open = %+v, %v; want the plugin's capabilities", info, ok)
	}
	if n := starts(t, path); n != 1 {
		t.Errorf("Open started the plugin %d times, want 1", n)
	}
}

func TestUnadvertisedCapabilityNeverReachesThePlugin(t *testing.T) {
	f := setup(t, baseEnv("sneaky"))
	ctx := ctxT(t, 20*time.Second)
	conn := open(t, f, driver.ConnConfig{})
	if _, ok := driver.As[driver.Mutator](conn); ok {
		t.Error("a capability missing from Info was unlocked by Open")
	}
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	row := driver.Object{{Key: "id", Value: driver.Int(99)}}
	if _, err := conn.(driver.Mutator).Insert(ctx, ref, row); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Insert = %v, want ErrUnsupported", err)
	}
	if err := conn.(driver.Mutator).Delete(ctx, ref, row); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Delete = %v, want ErrUnsupported", err)
	}
	tx, err := conn.(driver.Transactor).Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, ok := driver.As[driver.Mutator](tx); ok {
		t.Error("a Tx offers mutator its Conn does not")
	}
	if _, err := tx.(driver.Mutator).Insert(ctx, ref, row); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Tx Insert = %v, want ErrUnsupported", err)
	}
	_ = tx.Rollback(ctx)
	rows, err := collect(ctx, conn, "!rpcs")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("mutating RPCs reached the plugin: %v", rows)
	}
}

// copyPlugin copies the test plugin into dir under the plugin name.
func copyPlugin(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(pluginBin)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, filepath.Base(pluginBin))
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

func registerDiscovered(t *testing.T, dir string) (*driver.Registry, *host.Host) {
	t.Helper()
	ps := host.Discover([]string{dir})
	if len(ps) != 1 {
		t.Fatalf("Discover = %v", ps)
	}
	reg := driver.NewRegistry()
	h := host.Register(reg, ps, host.Options{Environ: baseEnv("")})
	t.Cleanup(func() { _ = h.Close() })
	return reg, h
}

func TestBinaryChangedSinceDiscoveryIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file semantics")
	}
	refused := func(t *testing.T, reg *driver.Registry) {
		t.Helper()
		_, err := reg.Open(ctxT(t, 20*time.Second), driver.ConnConfig{Driver: pluginName})
		if !errors.Is(err, driver.ErrConnection) || !strings.Contains(err.Error(), "changed since discovery") {
			t.Errorf("Open of a changed binary = %v, want a changed-since-discovery ErrConnection", err)
		}
	}
	t.Run("rewritten in place", func(t *testing.T) {
		dir := t.TempDir()
		bin := copyPlugin(t, dir)
		before, err := os.Stat(bin)
		if err != nil {
			t.Fatal(err)
		}
		reg, _ := registerDiscovered(t, dir)
		// Same inode, mode and size, different bytes: an O_TRUNC rewrite,
		// as `cp new old` does.
		data, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)-1] ^= 0xff
		fh, err := os.OpenFile(bin, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.Write(data); err != nil {
			t.Fatal(err)
		}
		_ = fh.Close()
		// Pin the modification time one second off, so the change shows
		// even on a file system with coarse timestamps.
		later := before.ModTime().Add(time.Second)
		if err := os.Chtimes(bin, later, later); err != nil {
			t.Fatal(err)
		}
		after, _ := os.Stat(bin)
		if !os.SameFile(before, after) || after.Size() != before.Size() || after.Mode() != before.Mode() {
			t.Fatal("the rewrite changed the inode, size or mode; the test no longer isolates the content check")
		}
		refused(t, reg)
	})
	t.Run("replaced by rename", func(t *testing.T) {
		dir := t.TempDir()
		bin := copyPlugin(t, dir)
		reg, _ := registerDiscovered(t, dir)
		other := t.TempDir()
		tmp := copyPlugin(t, other)
		if err := os.Rename(tmp, bin); err != nil {
			t.Fatal(err)
		}
		refused(t, reg)
	})
}

func TestBinaryMadeWritableSinceDiscoveryIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file semantics")
	}
	dir := t.TempDir()
	bin := copyPlugin(t, dir)
	reg, _ := registerDiscovered(t, dir)
	if err := os.Chmod(bin, 0o757); err != nil {
		t.Fatal(err)
	}
	_, err := reg.Open(ctxT(t, 20*time.Second), driver.ConnConfig{Driver: pluginName})
	if !errors.Is(err, driver.ErrConnection) || !strings.Contains(err.Error(), "world-writable") {
		t.Errorf("Open of a world-writable binary = %v", err)
	}
}

func TestUnchangedDiscoveredBinaryStarts(t *testing.T) {
	dir := t.TempDir()
	copyPlugin(t, dir)
	reg, _ := registerDiscovered(t, dir)
	c, err := reg.Open(ctxT(t, 20*time.Second), driver.ConnConfig{Driver: pluginName})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = c.Close()
}

// Open must not hand back a Conn without the plugin's Info: when Info
// fails, or ctx ends while it loads, the plugin-side Conn is closed and
// Open fails.
func TestOpenFailsWhenInfoFails(t *testing.T) {
	f := setup(t, baseEnv("bad-info-once"))
	_, err := f.reg.Open(ctxT(t, 20*time.Second), driver.ConnConfig{Driver: pluginName})
	if !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("Open with a bad Info = %v, want ErrLimit", err)
	}
	conn := open(t, f, driver.ConnConfig{})
	if _, ok := driver.As[driver.Browser](conn); !ok {
		t.Error("the second Open, with a good Info, lacks the plugin's capabilities")
	}
	if got := closes(t, conn); got != "1" {
		t.Errorf("plugin saw %s Close RPCs, want 1 (the failed Open's Conn)", got)
	}
}

func TestOpenFailsWhenCtxEndsDuringInfo(t *testing.T) {
	f := setup(t, baseEnv("slow-info-once"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := f.reg.Open(ctx, driver.ConnConfig{Driver: pluginName})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open whose Info outlives ctx = %v, want DeadlineExceeded", err)
	}
	conn := open(t, f, driver.ConnConfig{})
	if got := closes(t, conn); got != "1" {
		t.Errorf("plugin saw %s Close RPCs, want 1 (the failed Open's Conn)", got)
	}
}

func closes(t *testing.T, conn driver.Conn) string {
	t.Helper()
	rows, err := collect(ctxT(t, 20*time.Second), conn, "!closes")
	if err != nil || len(rows) != 1 {
		t.Fatalf("!closes = %v, %v", rows, err)
	}
	s, _ := rows[0][0].AsText()
	return s
}
