package duckdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// newFile creates a DuckDB database at a fresh path with stmts run on it,
// closes it and returns the path.
func newFile(t *testing.T, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.duckdb")
	c := open(t, path, false)
	for _, s := range stmts {
		if _, err := queryAll(t, c, driver.Query{Text: s}); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func open(t *testing.T, path string, readOnly bool) driver.Conn {
	t.Helper()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://" + path, ReadOnly: readOnly})
	if err != nil {
		t.Fatalf("Open(%s, readOnly=%v): %v", path, readOnly, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func queryAll(t *testing.T, q driver.Querier, query driver.Query) ([]driver.Row, error) {
	t.Helper()
	rs, err := q.Query(context.Background(), query)
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(context.Background(), rs, 0)
	return rows, err
}

func one(t *testing.T, q driver.Querier, text string) driver.Value {
	t.Helper()
	rows, err := queryAll(t, q, driver.Query{Text: text})
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: got %v, want one value", text, rows)
	}
	return rows[0][0]
}

func TestOpenReadOnlyMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.duckdb")
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://" + path, ReadOnly: true})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("read-only Open of a missing file = %v, want ErrNotFound", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a read-only Open created %s", path)
	}
}

func TestOpenReadOnlyMemoryRefused(t *testing.T) {
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://:memory:", ReadOnly: true})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("read-only :memory: = %v, want ErrUnsupported", err)
	}
}

func TestMemoryDatabase(t *testing.T) {
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://:memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := queryAll(t, c, driver.Query{Text: "create table m (a int)"}); err != nil {
		t.Fatal(err)
	}
	if v := one(t, c, "select count(*) from m"); v.String() != "0" {
		t.Errorf("count = %v", v)
	}
}

// A dedicated read-only instance carries the protocol layers whatever the
// lexical guard decides: access_mode=READ_ONLY, external access off and a
// locked configuration.
func TestReadOnlyInstanceSettings(t *testing.T) {
	path := newFile(t, "create table t (a int)", "insert into t values (1)")
	ro := open(t, path, true)
	for setting, want := range map[string]string{
		"access_mode":                  "read_only",
		"enable_external_access":       "false",
		"lock_configuration":           "true",
		"autoinstall_known_extensions": "false",
		"autoload_known_extensions":    "false",
	} {
		if got := one(t, ro, "select current_setting('"+setting+"')::varchar"); got.String() != want {
			t.Errorf("%s = %v, want %s", setting, got, want)
		}
	}
	c := ro.(*conn)
	if c.shared {
		t.Fatal("the read-only conn shares a read-write instance, want its own read-only one")
	}
	// Past the lexical guard, straight to the database: each layer refuses.
	// (DuckDB allows TEMP objects on a read-only instance; the guard and the
	// statement-type check refuse CREATE.)
	ctx := context.Background()
	for _, q := range []string{
		"insert into t values (2)",
		"set enable_external_access = true",
		"set allow_unsigned_extensions = true",
		"set lock_configuration = false",
		"select * from read_csv('" + path + "')",
		"copy t to '" + filepath.Join(t.TempDir(), "out.csv") + "'",
		"attach '" + filepath.Join(t.TempDir(), "other.duckdb") + "' as o",
		"install httpfs",
	} {
		if _, err := c.db.ExecContext(ctx, q); err == nil {
			t.Errorf("%q ran on the read-only instance", q)
		}
	}
	if v := one(t, ro, "select count(*) from t"); v.String() != "1" {
		t.Errorf("count = %v, want 1", v)
	}
}

// With a read-write instance of the same file already open in the process,
// DuckDB refuses a second instance with another configuration, so a
// read-only conn shares it. Its queries then run in a READ ONLY transaction
// behind the parser's statement-type check.
func TestReadOnlySharesReadWriteInstance(t *testing.T) {
	path := newFile(t, "create table t (a int)", "create sequence s", "insert into t values (1)")
	rw := open(t, path, false)
	ro := open(t, path, true)
	c := ro.(*conn)
	if !c.shared {
		t.Fatal("want the read-only conn to share the read-write instance")
	}
	if _, err := queryAll(t, rw, driver.Query{Text: "insert into t values (2)"}); err != nil {
		t.Fatal(err)
	}
	if v := one(t, ro, "select count(*) from t"); v.String() != "2" {
		t.Errorf("the read-only conn sees count %v, want 2 (the read-write conn's write)", v)
	}
	for _, q := range []string{
		"insert into t values (3)",
		"create table x (a int)",
		"create temp table x (a int)",
		"select nextval('s')", // a SELECT that writes: the READ ONLY transaction refuses it
	} {
		_, err := queryAll(t, roRunner{c}, driver.Query{Text: q, ReadOnly: true})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q past the lexical guard = %v, want ErrReadOnly", q, err)
		}
	}
	if v := one(t, rw, "select count(*) from t"); v.String() != "2" {
		t.Errorf("count = %v after refused writes, want 2", v)
	}
	// The read-write conn's per-query read-only flag takes the same path.
	_, err := queryAll(t, roRunner{rw.(*conn)}, driver.Query{Text: "select nextval('s')", ReadOnly: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("nextval with Query.ReadOnly past the lexical guard = %v, want ErrReadOnly", err)
	}
}

// roRunner runs queries on c's read-only path without the lexical guard,
// so a test can reach the layers behind it.
type roRunner struct{ c *conn }

func (r roRunner) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return r.c.readOnlyQuery(ctx, q)
}

func TestReadWriteRefusedWhileReadOnlyInstanceOpen(t *testing.T) {
	path := newFile(t, "create table t (a int)")
	_ = open(t, path, true)
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://" + path})
	if !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("read-write Open while a read-only instance is open = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("the error does not explain the conflict: %v", err)
	}
}

func TestReadOnlyRefusalSendsNothing(t *testing.T) {
	path := newFile(t, "create table t (a int)", "insert into t values (1)")
	rw := open(t, path, false)
	for _, q := range []string{"delete from t", "select 1; delete from t", "copy t to 'x.csv'"} {
		_, err := queryAll(t, rw, driver.Query{Text: q, ReadOnly: true})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q with Query.ReadOnly = %v, want ErrReadOnly", q, err)
		}
	}
	if v := one(t, rw, "select count(*) from t"); v.String() != "1" {
		t.Errorf("count = %v, want 1", v)
	}
	if _, err := os.Stat("x.csv"); err == nil {
		t.Error("a refused COPY wrote x.csv")
	}
}

func TestTxControlRefusedOutsideTx(t *testing.T) {
	path := newFile(t)
	rw := open(t, path, false)
	for _, q := range []string{"begin", "commit", "rollback", "begin transaction read only"} {
		if _, err := queryAll(t, rw, driver.Query{Text: q}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%q through Query = %v, want ErrUnsupported (use Begin)", q, err)
		}
	}
}

func TestMultipleStatementsRefused(t *testing.T) {
	path := newFile(t, "create table t (a int)")
	rw := open(t, path, false)
	_, err := queryAll(t, rw, driver.Query{Text: "insert into t values (1); insert into t values (2)"})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("two statements = %v, want ErrUnsupported", err)
	}
	if v := one(t, rw, "select count(*) from t"); v.String() != "0" {
		t.Errorf("count = %v, want 0", v)
	}
}
