package host_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/plugin/convert"
	"github.com/xavidop/tobira/plugin/host"
)

// pluginName is the driver name of the test plugin binary.
const pluginName = "testplug"

// pluginBin is the test plugin (testdata/plugin), built once by TestMain.
var pluginBin string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "tobira-host-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	pluginBin = filepath.Join(dir, "tobira-driver-"+pluginName+exe)
	cmd := exec.Command("go", "build", "-o", pluginBin, ".")
	cmd.Dir = filepath.Join("testdata", "plugin")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build the test plugin: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

// logBuffer is a concurrency-safe buffer for a JSON slog handler.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fixture struct {
	reg  *driver.Registry
	host *host.Host
	drv  driver.Driver
	log  *logBuffer
}

// baseEnv is the environment the test plugin runs with: enough to start.
func baseEnv(mode string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if runtime.GOOS == "windows" {
		env = append(env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	if mode != "" {
		env = append(env, "TOBIRA_PLUGIN_TEST_MODE="+mode)
	}
	return env
}

// setup registers the test plugin in a fresh registry with env as the
// host's environment.
func setup(t *testing.T, env []string) fixture {
	t.Helper()
	logs := &logBuffer{}
	reg := driver.NewRegistry()
	h := host.Register(reg, []host.Plugin{{Name: pluginName, Path: pluginBin, Dir: filepath.Dir(pluginBin)}}, host.Options{
		Logger:       slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Environ:      env,
		StartTimeout: 10 * time.Second,
	})
	t.Cleanup(func() { _ = h.Close() })
	d, ok := reg.Lookup(pluginName)
	if !ok {
		t.Fatalf("plugin %q not registered; conflicts: %v", pluginName, h.Conflicts())
	}
	return fixture{reg: reg, host: h, drv: d, log: logs}
}

// checkNoLeak closes the host and fails on goroutines left since before.
func checkNoLeak(t *testing.T, f fixture, before goleak.Option) {
	t.Helper()
	_ = f.host.Close()
	if err := goleak.Find(before); err != nil {
		t.Errorf("goroutines leaked: %v", err)
	}
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func open(t *testing.T, f fixture, cfg driver.ConnConfig) driver.Conn {
	t.Helper()
	if cfg.Driver == "" {
		cfg.Driver = pluginName
	}
	c, err := f.reg.Open(ctxT(t, 20*time.Second), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func collect(ctx context.Context, conn driver.Querier, text string) ([]driver.Row, error) {
	rs, err := conn.Query(ctx, driver.Query{Text: text})
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	return rows, err
}

var people = driver.CollectionRef{Namespace: "main", Name: "tobira_people"}

func TestSmokeThroughTheHost(t *testing.T) {
	f := setup(t, baseEnv(""))
	ctx := ctxT(t, 30*time.Second)

	info, err := f.host.Info(ctx, pluginName)
	if err != nil {
		t.Fatalf("Host.Info: %v", err)
	}
	if got := f.drv.Info(); !reflect.DeepEqual(got, info) {
		t.Errorf("Info() after Host.Info = %+v, want the cached %+v", got, info)
	}
	fake := drivertest.NewFake(drivertest.WithPeople()).Driver().Info()
	if info.Name != pluginName {
		t.Errorf("Info().Name = %q, want the registry name %q", info.Name, pluginName)
	}
	if info.Kind != fake.Kind || info.Dialect != fake.Dialect || info.Version != fake.Version {
		t.Errorf("Info() = %+v, want the plugin's kind, dialect and version from %+v", info, fake)
	}
	if !slices.Equal(info.Capabilities, fake.Capabilities) {
		t.Errorf("Info().Capabilities = %v, want %v", info.Capabilities, fake.Capabilities)
	}
	if !slices.Contains(info.Schemes, pluginName) || !slices.Contains(info.Schemes, "fake") {
		t.Errorf("Info().Schemes = %v, want the registry name and the plugin's schemes", info.Schemes)
	}
	if d, ok := f.reg.ForScheme(pluginName); !ok || d != f.drv {
		t.Errorf("ForScheme(%q) does not route to the plugin", pluginName)
	}

	conn := open(t, f, driver.ConnConfig{DSN: "fake://smoke"})
	got := driver.CapabilitiesOf(conn)
	var want []driver.Capability
	for _, c := range info.Capabilities {
		if c != driver.CapReadOnly {
			want = append(want, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("CapabilitiesOf(conn) = %v, want %v", got, want)
	}
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	rows, err := collect(ctx, conn, "select * from tobira_people order by id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != len(drivertest.People.Rows) {
		t.Errorf("query returned %d rows, want %d", len(rows), len(drivertest.People.Rows))
	}
	schema, err := conn.Describe(ctx, people)
	if err != nil || len(schema.Fields) != len(drivertest.People.Columns) {
		t.Fatalf("Describe = %+v, %v", schema, err)
	}
	b, ok := driver.As[driver.Browser](conn)
	if !ok {
		t.Fatal("conn is not a Browser")
	}
	rs, err := b.Browse(ctx, people, driver.BrowseOptions{Limit: 2})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	page, st, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(page) != 2 || st.NextCursor == "" {
		t.Errorf("Browse page = %d rows, cursor %q, %v", len(page), st.NextCursor, err)
	}
	tr, ok := driver.As[driver.Transactor](conn)
	if !ok {
		t.Fatal("conn is not a Transactor")
	}
	tx, err := tr.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, ok := driver.As[driver.Mutator](tx); !ok {
		t.Error("the Fake's Tx offers mutator, but the proxy does not")
	}
	if _, err := collect(ctx, tx, "delete from tobira_people where id = 1"); err != nil {
		t.Fatalf("tx write: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rows, _ := collect(ctx, conn, "select * from tobira_people"); len(rows) != len(drivertest.People.Rows) {
		t.Errorf("after Rollback %d rows, want %d", len(rows), len(drivertest.People.Rows))
	}
	if err := conn.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := conn.Ping(ctx); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Ping after Close = %v, want ErrConnection", err)
	}
}

func TestDrivertestThroughTheHost(t *testing.T) {
	f := setup(t, baseEnv(""))
	// Load the real Info (and start the plugin) before the suite reads Info
	// and takes its leak snapshot.
	if _, err := f.host.Info(ctxT(t, 20*time.Second), pluginName); err != nil {
		t.Fatal(err)
	}
	openFn := func(ro bool) func(testing.TB) driver.Conn {
		return func(tb testing.TB) driver.Conn {
			c, err := f.reg.Open(context.Background(), driver.ConnConfig{Driver: pluginName, DSN: "fake://suite", ReadOnly: ro})
			if err != nil {
				tb.Fatalf("open: %v", err)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       f.drv,
		Open:         openFn(false),
		OpenReadOnly: openFn(true),
		Fixture: drivertest.Fixture{
			Namespace:        "main",
			Collection:       "tobira_people",
			SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
			Missing:          driver.Query{Text: "select * from nope"},
			Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
			Slow:             driver.Query{Text: "sleep 10s"},
			ParamQuery:       driver.Query{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(7)}},
			AutoIncrementKey: drivertest.Bool(true),
			Vector:           drivertest.KindVectorFixture(driver.CollectionRef{Namespace: "main", Name: drivertest.VectorCollection}),
		},
	})
}

func TestRegisterDoesNotLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell wrapper")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	wrapper := filepath.Join(dir, "tobira-driver-"+pluginName)
	script := fmt.Sprintf("#!/bin/sh\necho launched >> %q\nexec %q \"$@\"\n", marker, pluginBin)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	plugins := host.Discover([]string{dir})
	if len(plugins) != 1 {
		t.Fatalf("Discover = %v", plugins)
	}
	reg := driver.NewRegistry()
	if err := reg.Add(drivertest.NewFake().Driver()); err != nil {
		t.Fatal(err)
	}
	h := host.Register(reg, plugins, host.Options{Environ: baseEnv("")})
	defer func() { _ = h.Close() }()

	// Using another driver never starts the plugin.
	c, err := reg.Open(context.Background(), driver.ConnConfig{DSN: "fake://x"})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, ok := reg.Lookup(pluginName); !ok {
		t.Fatal("plugin not registered")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Register (or a Lookup) launched the plugin")
	}
	c = open(t, fixture{reg: reg}, driver.ConnConfig{DSN: "fake://x"})
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("Open did not launch the plugin")
	}
	_ = c.Close()
}

func TestCookieMismatchIsAClearError(t *testing.T) {
	f := setup(t, baseEnv("cookie"))
	_, err := f.reg.Open(ctxT(t, 20*time.Second), driver.ConnConfig{Driver: pluginName})
	if !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("Open = %v, want ErrConnection", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "magic cookie") || !strings.Contains(msg, pluginName) {
		t.Errorf("error %q should name the plugin and the cookie mismatch", msg)
	}
	if _, err := f.host.Info(ctxT(t, 20*time.Second), pluginName); err == nil {
		t.Error("Host.Info should report the failed start")
	}
}

func TestProtocolVersionMismatchIsAClearError(t *testing.T) {
	f := setup(t, baseEnv("proto2"))
	_, err := f.reg.Open(ctxT(t, 20*time.Second), driver.ConnConfig{Driver: pluginName})
	if !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("Open = %v, want ErrConnection", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "protocol version") || !strings.Contains(msg, "2") {
		t.Errorf("error %q should say the plugin speaks protocol version 2", msg)
	}
	// Info falls back to what the file name says.
	if info := f.drv.Info(); info.Name != pluginName || info.Kind != driver.KindOther {
		t.Errorf("Info() of a plugin that cannot start = %+v", info)
	}
}

func TestPluginEnvironmentIsAllowListed(t *testing.T) {
	secrets := map[string]string{
		"TOBIRA_CONN_X_PASSWORD":   "conn-pw-1234",
		"GEMINI_API_KEY":           "gemini-key-1234",
		"FOO_TOKEN":                "foo-token-1234",
		"TOBIRA_PLUGIN_FOO_SECRET": "plugin-secret-1234",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("TOBIRA_PLUGIN_FOO", "visible")
	t.Setenv("TOBIRA_PLUGIN_TEST_MODE", "")
	// The host's whole environment goes in: only the allow-list may come out.
	f := setup(t, os.Environ())
	conn := open(t, f, driver.ConnConfig{})
	rows, err := collect(ctxT(t, 20*time.Second), conn, "!env")
	if err != nil {
		t.Fatalf("!env: %v", err)
	}
	env := map[string]string{}
	for _, r := range rows {
		s, _ := r[0].AsText()
		k, v, _ := strings.Cut(s, "=")
		env[k] = v
	}
	for k, v := range secrets {
		if _, ok := env[k]; ok {
			t.Errorf("the plugin sees %s", k)
		}
		for ek, ev := range env {
			if strings.Contains(ev, v) {
				t.Errorf("the plugin sees the value of %s in %s", k, ek)
			}
		}
	}
	if env["TOBIRA_PLUGIN_FOO"] != "visible" {
		t.Errorf("TOBIRA_PLUGIN_FOO = %q, want it passed through", env["TOBIRA_PLUGIN_FOO"])
	}
	if env["PATH"] != os.Getenv("PATH") {
		t.Errorf("PATH not passed through")
	}
	for k := range env {
		allowed := slices.Contains([]string{"PATH", "HOME", "TMPDIR", "LANG", "TZ", "SystemRoot", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "PATHEXT"}, k) ||
			strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "TOBIRA_PLUGIN_") || strings.HasPrefix(k, "PLUGIN_")
		if !allowed {
			t.Errorf("the plugin sees %s, which is not on the allow-list", k)
		}
	}
}

func TestCrashMidQueryThenRestart(t *testing.T) {
	before := goleak.IgnoreCurrent()
	f := setup(t, baseEnv(""))
	defer checkNoLeak(t, f, before)
	ctx := ctxT(t, 30*time.Second)
	conn := open(t, f, driver.ConnConfig{})
	rs, err := conn.Query(ctx, driver.Query{Text: "!crash"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Read the first frame, so the crash below is mid-stream.
	for range convert.RowsPerFrame {
		if _, ok, err := rs.Next(ctx); !ok || err != nil {
			t.Fatalf("Next before the crash: %v, %v", ok, err)
		}
	}
	// Kill the plugin from a second call; that call fails too.
	if _, err := conn.Query(ctx, driver.Query{Text: "!exit"}); !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("the query that kills the plugin = %v, want ErrConnection", err)
	}
	_, _, err = driver.Collect(ctx, rs, 0)
	if !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("reading from a crashed plugin = %v, want ErrConnection", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("a crash must not look like a cancellation: %v", err)
	}
	if err := conn.Ping(ctx); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Ping on a Conn of the dead process = %v, want ErrConnection", err)
	}

	// The next Open restarts the plugin.
	conn2 := open(t, f, driver.ConnConfig{})
	rows, err := collect(ctx, conn2, "select * from tobira_people")
	if err != nil || len(rows) != len(drivertest.People.Rows) {
		t.Fatalf("query after restart = %d rows, %v", len(rows), err)
	}
	// The old Conn stays dead.
	if err := conn.Ping(ctx); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("old Conn after restart = %v, want ErrConnection", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("closing a dead Conn = %v, want nil", err)
	}
}

func TestCancelStopsAHungCall(t *testing.T) {
	before := goleak.IgnoreCurrent()
	f := setup(t, baseEnv(""))
	defer checkNoLeak(t, f, before)
	conn := open(t, f, driver.ConnConfig{})

	// A query that blocks in the driver.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := collect(ctx, conn, "sleep 1m")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hung query = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the cancel took %s", d)
	}

	// A stream that stops sending mid-way.
	rs, err := conn.Query(context.Background(), driver.Query{Text: "!hang"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = rs.Close() }()
	for range convert.RowsPerFrame {
		if _, ok, err := rs.Next(context.Background()); !ok || err != nil {
			t.Fatalf("Next: %v, %v", ok, err)
		}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	start = time.Now()
	if _, _, err := rs.Next(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Next on a hung stream = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the cancel took %s", d)
	}
	// The plugin is still healthy.
	if err := conn.Ping(context.Background()); err != nil {
		t.Errorf("Ping after cancels: %v", err)
	}
}

func TestStalledStartHonoursContext(t *testing.T) {
	before := goleak.IgnoreCurrent()
	f := setup(t, baseEnv("stall"))
	defer checkNoLeak(t, f, before)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.reg.Open(ctx, driver.ConnConfig{Driver: pluginName})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Open of a stalled plugin = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Open took %s", d)
	}
	start = time.Now()
	if err := f.host.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Close of a stalled plugin took %s", d)
	}
	if _, err := f.reg.Open(context.Background(), driver.ConnConfig{Driver: pluginName}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Open after Close = %v, want ErrConnection", err)
	}
}

func TestHostileAnswersAreCapped(t *testing.T) {
	f := setup(t, baseEnv("hostile"))
	ctx := ctxT(t, 30*time.Second)
	conn := open(t, f, driver.ConnConfig{DSN: "fake://u:s3cr3t-pw@h/db", Password: driver.NewSecret("other-pw-99")})

	for _, c := range driver.CapabilitiesOf(conn) {
		if !slices.Contains(drivertest.NewFake().Driver().Info().Capabilities, c) {
			t.Errorf("junk capability %q trusted", c)
		}
	}
	for _, q := range []string{"huge value", "too many columns"} {
		_, err := collect(ctx, conn, q)
		if !errors.Is(err, convert.ErrLimit) {
			t.Errorf("%s: %v, want ErrLimit", q, err)
		}
	}
	if _, err := collect(ctx, conn, "over message cap"); err == nil {
		t.Error("a frame over the message cap was accepted")
	}
	if _, err := conn.Namespaces(ctx); !errors.Is(err, convert.ErrLimit) {
		t.Errorf("oversized namespace name: %v, want ErrLimit", err)
	}
	_, err := conn.Collections(ctx, driver.NamespaceRef{Name: "main"})
	if !errors.Is(err, driver.ErrNotFound) || len(err.Error()) > convert.MaxNameBytes+256 {
		t.Errorf("a 1 MiB error message: kind %v, %d bytes", driver.KindName(err), len(err.Error()))
	}
	// The plugin leaked the credentials: the host masks them again.
	err = conn.Ping(ctx)
	if !errors.Is(err, driver.ErrAuth) {
		t.Fatalf("Ping = %v, want ErrAuth", err)
	}
	if msg := err.Error(); strings.Contains(msg, "s3cr3t-pw") || strings.Contains(msg, "other-pw-99") {
		t.Errorf("host did not redact the plugin's error: %q", msg)
	}
	// The plugin is still usable after all that.
	if rows, err := collect(ctx, conn, "select * from tobira_people"); err != nil || len(rows) == 0 {
		t.Errorf("query after hostile answers: %d rows, %v", len(rows), err)
	}
}

func TestBadInfoFallsBackToTheFileName(t *testing.T) {
	f := setup(t, baseEnv("bad-info"))
	info := f.drv.Info()
	if info.Name != pluginName || info.Kind != driver.KindOther || len(info.Capabilities) != 0 {
		t.Errorf("Info() = %+v, want the file-name fallback", info)
	}
	if _, err := f.host.Info(ctxT(t, 20*time.Second), pluginName); !errors.Is(err, convert.ErrLimit) {
		t.Errorf("Host.Info = %v, want ErrLimit", err)
	}
}

func TestBuiltinWinsAConflict(t *testing.T) {
	reg := driver.NewRegistry()
	builtin := drivertest.NewFake().Driver()
	if err := reg.Add(builtin); err != nil {
		t.Fatal(err)
	}
	clash := host.Plugin{Name: "fake", Path: pluginBin, Dir: filepath.Dir(pluginBin)}
	ok := host.Plugin{Name: pluginName, Path: pluginBin, Dir: filepath.Dir(pluginBin)}
	h := host.Register(reg, []host.Plugin{clash, ok}, host.Options{Environ: baseEnv("")})
	defer func() { _ = h.Close() }()

	if d, _ := reg.Lookup("fake"); d != builtin {
		t.Error("a plugin replaced the built-in driver")
	}
	if _, isPlugin := host.PluginOf(builtin); isPlugin {
		t.Error("PluginOf(builtin) reports a plugin")
	}
	cs := h.Conflicts()
	if len(cs) != 1 || cs[0].Plugin != clash || !strings.Contains(cs[0].Reason, "built-in") {
		t.Errorf("Conflicts() = %+v, want one for the built-in fake", cs)
	}
	if got := h.Plugins(); len(got) != 1 || got[0] != ok {
		t.Errorf("Plugins() = %+v, want only %s", got, pluginName)
	}
	d, found := reg.Lookup(pluginName)
	if p, isPlugin := host.PluginOf(d); !found || !isPlugin || p != ok {
		t.Errorf("PluginOf(%s) = %+v, %v", pluginName, p, isPlugin)
	}
	// Registering the same plugins again is a no-op, not a conflict.
	h2 := host.Register(reg, []host.Plugin{ok}, host.Options{Environ: baseEnv("")})
	defer func() { _ = h2.Close() }()
	if len(h2.Conflicts()) != 0 || len(h2.Plugins()) != 0 {
		t.Errorf("re-registering: plugins %v, conflicts %v", h2.Plugins(), h2.Conflicts())
	}
}

func TestVectorTextIsEmbeddedByTheHost(t *testing.T) {
	f := setup(t, baseEnv(""))
	ctx := ctxT(t, 20*time.Second)
	ref := driver.CollectionRef{Namespace: "main", Name: drivertest.VectorCollection}
	dims := len(drivertest.KindVectorFixture(ref).Query.Vector)
	emb := &countingEmbedder{HashEmbedder: drivertest.HashEmbedder{Dims: dims}}
	conn := open(t, f, driver.ConnConfig{Embedder: emb})
	vs, ok := driver.As[driver.VectorSearcher](conn)
	if !ok {
		t.Fatal("conn is not a VectorSearcher")
	}
	rs, err := vs.Similar(ctx, ref, driver.VectorQuery{Text: "find me", K: 2})
	if err != nil {
		t.Fatalf("Similar(text): %v", err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) != 2 {
		t.Errorf("Similar(text) = %d rows, %v", len(rows), err)
	}
	if got := emb.texts(); !slices.Equal(got, []string{"find me"}) {
		t.Errorf("embedder saw %q, want [find me]", got)
	}

	// Without an embedder the plugin says it needs one.
	plain := open(t, f, driver.ConnConfig{})
	vs, _ = driver.As[driver.VectorSearcher](plain)
	if _, err := vs.Similar(ctx, ref, driver.VectorQuery{Text: "find me", K: 2}); !errors.Is(err, driver.ErrUnsupported) ||
		!strings.Contains(err.Error(), driver.NeedEmbedderHint) {
		t.Errorf("Similar(text) without an embedder = %v", err)
	}
}

type countingEmbedder struct {
	drivertest.HashEmbedder
	mu  sync.Mutex
	got []string
}

func (e *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.got = append(e.got, texts...)
	e.mu.Unlock()
	return e.HashEmbedder.Embed(ctx, texts)
}

func (e *countingEmbedder) texts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.got)
}

func TestPluginStderrGoesToTheDebugLog(t *testing.T) {
	f := setup(t, append(baseEnv(""), "TOBIRA_PLUGIN_LOG_LEVEL=debug"))
	conn := open(t, f, driver.ConnConfig{})
	_ = conn.Close()
	_ = f.host.Close()
	logs := f.log.String()
	if !strings.Contains(logs, `"msg":"plugin stderr"`) || !strings.Contains(logs, `"level":"DEBUG"`) {
		t.Errorf("no plugin stderr in the debug log:\n%s", logs)
	}
}

// Compile-time check that the test helpers stay in use.
var _ io.Writer = (*logBuffer)(nil)
