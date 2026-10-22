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
	"github.com/xavidop/tobira/drivers/duckdb/internal/duckdb"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/plugin/host"
)

// fixture is the conformance input, the same in process and through the
// plugin host.
var fixture = drivertest.Fixture{
	Namespace:        "main",
	Collection:       drivertest.PeopleCollection(),
	AutoIncrementKey: drivertest.Bool(false),
	SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
	Missing:          driver.Query{Text: "select * from nope"},
	Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
	// The driver runs one statement per call; these hide a write in forms
	// DuckDB accepts, plus file and settings side effects.
	CompoundWrites: []driver.Query{
		{Text: "select 1; delete from tobira_people where id = 1"},
		{Text: "insert into tobira_people (id, name, email) values (999, 'n', 'e@x') returning id"},
		{Text: "with x as (select 1) delete from tobira_people where id = 1"},
		{Text: "update tobira_people set name = 'x' where id = 1 returning *"},
		{Text: "create table tobira_people_copy as select * from tobira_people"},
		{Text: "copy tobira_people to 'tobira_people.csv'"},
		{Text: "attach ':memory:' as m"},
		{Text: "set enable_external_access = true"},
	},
	Slow:       driver.Query{Text: "select count(*) from range(100000000) a, range(1000) b where a.range + b.range = -1"},
	ParamQuery: driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(7)}},
	TxWrite:    driver.Query{Text: "update tobira_people set name = 'tx' where id = 1"},
	TxRestore:  driver.Query{Text: "update tobira_people set name = 'Ōtani Shōhei' where id = 1"},
	CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
}

// seedFile writes the People fixture into a fresh DuckDB file and returns
// its path. The DuckDB types follow the PostgreSQL seed: DECIMAL(10,2),
// TIMESTAMPTZ and JSON.
func seedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "people.duckdb")
	c, err := duckdb.New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://" + path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for _, s := range seedSQL() {
		rs, err := c.Query(context.Background(), driver.Query{Text: s})
		if err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
		if _, _, err := driver.Collect(context.Background(), rs, 0); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func seedSQL() []string {
	stmts := drivertest.SQLSeed("postgres")
	stmts[0] = "CREATE TABLE tobira_people (id INTEGER PRIMARY KEY, name VARCHAR NOT NULL, email VARCHAR NOT NULL UNIQUE, " +
		"age INTEGER, score DECIMAL(10,2), active BOOLEAN, created_at TIMESTAMPTZ, tags JSON)"
	return stmts
}

// conformance runs drivertest Strict against d on the database at path.
func conformance(t *testing.T, d driver.Driver, path string) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := d.Open(context.Background(), driver.ConnConfig{Name: "people", DSN: "duckdb://" + path, ReadOnly: readOnly})
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
	if _, err := os.Stat("tobira_people.csv"); err == nil {
		_ = os.Remove("tobira_people.csv")
		t.Error("a refused COPY wrote tobira_people.csv")
	}
}

// TestConformanceInProcess is the reference: the driver as a Go value.
func TestConformanceInProcess(t *testing.T) {
	conformance(t, duckdb.New(), seedFile(t))
}

// TestConformanceReadOnlyFirst opens the read-only side before any
// read-write conn exists, so it gets a dedicated access_mode=READ_ONLY
// instance, and runs the read-only probes against it.
func TestConformanceReadOnlyFirst(t *testing.T) {
	path := seedFile(t)
	ro, err := duckdb.New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://" + path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	ctx := context.Background()
	rs, err := ro.Query(ctx, fixture.SelectAll)
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) != len(drivertest.People.Rows) {
		t.Fatalf("SelectAll on the read-only instance: %d rows, %v", len(rows), err)
	}
	for _, q := range append([]driver.Query{fixture.Write, fixture.TxWrite}, fixture.CompoundWrites...) {
		if _, err := ro.Query(ctx, q); err == nil {
			t.Errorf("%q ran on a read-only conn", q.Text)
		}
	}
}

// TestConformanceThroughTheHost builds this module's plugin binary,
// discovers and registers it the way tobira does at startup, and runs the
// same Strict suite against the registry's driver: every call crosses the
// plugin protocol into a separate process.
func TestConformanceThroughTheHost(t *testing.T) {
	path := seedFile(t)
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		if err := goleak.Find(before); err != nil {
			t.Errorf("goroutines left after Host.Close: %v", err)
		}
	})

	dir := buildPlugin(t)
	plugins := host.Discover([]string{dir})
	if len(plugins) != 1 || plugins[0].Name != duckdb.Name {
		t.Fatalf("Discover(%s) = %+v, want the duckdb plugin", dir, plugins)
	}
	reg := driver.NewRegistry()
	h := host.Register(reg, plugins, host.Options{})
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Host.Close: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := h.Info(ctx, duckdb.Name)
	if err != nil {
		t.Fatalf("Host.Info: %v", err)
	}
	if want := duckdb.New().Info(); !sameInfo(info, want) {
		t.Fatalf("Info through the host = %+v, want the driver's %+v", info, want)
	}
	d, ok := reg.Lookup(duckdb.Name)
	if !ok {
		t.Fatal("the registry has no duckdb driver")
	}
	if p, ok := host.PluginOf(d); !ok || p.Name != duckdb.Name {
		t.Fatalf("PluginOf(duckdb) = %+v, %v: want the plugin", p, ok)
	}
	conformance(t, d, path)
}

func sameInfo(a, b driver.Info) bool {
	return a.Name == b.Name && a.Kind == b.Kind && a.Dialect == b.Dialect && a.Version == b.Version &&
		slices.Equal(a.Schemes, b.Schemes) && slices.Equal(a.Capabilities, b.Capabilities)
}

// pluginFile is the binary name tobira discovers: tobira-driver-<name>.
func pluginFile() string {
	name := host.Prefix + duckdb.Name
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// buildPlugin builds this module's main package into a fresh 0700
// directory as tobira-driver-duckdb, mode 0755, and returns the directory.
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
// (no workspace). The plugin needs cgo.
func goBuild(t *testing.T, pkgDir, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkgDir, err, b)
	}
}
