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

// fileProbes are statements that read a file through a replacement scan in
// table positions a guard that looks only after FROM would miss. %s is a CSV
// path.
var fileProbes = []string{
	"SELECT * FROM (SUMMARIZE '%s')",
	"SELECT * FROM (DESCRIBE '%s')",
	"SELECT * FROM (SHOW '%s')",
	"SELECT * FROM (PIVOT '%s' ON a USING count(*))",
	"SELECT * FROM (UNPIVOT '%s' ON a)",
	"SELECT * FROM ONLY '%s'",
	"SELECT * FROM t JOIN ONLY '%s' ON true",
	`SELECT * FROM t UNION ALL TABLE "%s"`,
	`SELECT * FROM (TABLE "%s")`,
	"SELECT * FROM t, LATERAL (SUMMARIZE '%s')",
	"SELECT (SELECT count(*) FROM (DESCRIBE '%s'))",
	"FROM t SELECT * UNION ALL BY NAME (SUMMARIZE '%s')",
	"SELECT * FROM '%s'",
	"SELECT * FROM read_csv('%s')",
	"SELECT * FROM read_text('%s')",
	"SELECT * FROM glob('%s')",
}

func csvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret.csv")
	if err := os.WriteFile(p, []byte("a\n42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func probe(p, path string) string { return strings.ReplaceAll(p, "%s", path) }

// Every probe is refused by the lexical guard...
func TestFileProbesRefusedByGuard(t *testing.T) {
	path := csvFile(t)
	for _, p := range fileProbes {
		if err := checkReadOnly(probe(p, path)); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", probe(p, path), err)
		}
	}
}

// ...and fails at the engine with the guard bypassed, on every kind of
// instance a read-only query can reach: a read-write instance (Query with
// ReadOnly), a read-only conn sharing it, and a dedicated read-only one.
// Even the read-write path (no read-only flag) cannot read the file:
// external access is off on every instance unless the DSN opts in.
func TestFileProbesFailAtTheEngine(t *testing.T) {
	csv := csvFile(t)
	db := newFile(t, "create table t (a int)", "insert into t values (1)")
	rw := open(t, db, false)
	shared := open(t, db, true)
	if !shared.(*conn).shared {
		t.Fatal("want the read-only conn to share the read-write instance")
	}
	dedicated := open(t, newFile(t, "create table t (a int)"), true)
	paths := map[string]driver.Querier{
		"read-write Query{ReadOnly}": roRunner{rw.(*conn)},
		"shared read-only conn":      roRunner{shared.(*conn)},
		"dedicated read-only conn":   roRunner{dedicated.(*conn)},
		"read-write Query":           rwRunner{rw.(*conn)},
	}
	for name, q := range paths {
		for _, p := range fileProbes {
			text := probe(p, csv)
			rows, err := queryAll(t, q, driver.Query{Text: text, ReadOnly: true})
			if err == nil {
				t.Errorf("%s: %q read the file past the guard: %v", name, text, rows)
			}
		}
	}
	for setting, want := range map[string]string{
		"enable_external_access": "false", "autoinstall_known_extensions": "false",
		"autoload_known_extensions": "false", "lock_configuration": "true",
	} {
		if got := one(t, rw, "select current_setting('"+setting+"')::varchar"); got.String() != want {
			t.Errorf("read-write instance %s = %v, want %s", setting, got, want)
		}
	}
	if _, err := queryAll(t, rw, driver.Query{Text: "set enable_external_access = true"}); err == nil {
		t.Error("external access was re-enabled on a read-write instance")
	}
}

// rwRunner runs queries on c's read-write path without any guard.
type rwRunner struct{ c *conn }

func (r rwRunner) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	sc, err := r.c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	q.ReadOnly = false
	return runOn(ctx, sc, true, false, q)
}

// A writable DSN may opt in to external access; read-only queries are then
// refused on that instance, since the engine no longer stops file reads.
func TestExternalAccessOptIn(t *testing.T) {
	csv := csvFile(t)
	db := newFile(t, "create table t (a int)")
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://" + db + "?external_access=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if v := one(t, c, "select count(*) from read_csv('"+csv+"')"); v.String() != "1" {
		t.Errorf("read_csv with external access = %v, want 1 row", v)
	}
	hint := func(err error) bool {
		return errors.Is(err, driver.ErrReadOnly) && strings.Contains(err.Error(), "separate read-only connection")
	}
	if _, err := queryAll(t, c, driver.Query{Text: "select count(*) from t", ReadOnly: true}); !hint(err) {
		t.Errorf("Query{ReadOnly} on an external-access conn = %v, want ErrReadOnly with a hint", err)
	}
	if _, err := c.(driver.Explainer).Explain(context.Background(), driver.Query{Text: "select 1", ReadOnly: true}, driver.ExplainOptions{}); !hint(err) {
		t.Errorf("Explain{ReadOnly} on an external-access conn = %v, want ErrReadOnly with a hint", err)
	}
	if _, err := c.(driver.Transactor).Begin(context.Background(), driver.TxOptions{ReadOnly: true}); !hint(err) {
		t.Errorf("read-only Begin on an external-access conn = %v, want ErrReadOnly with a hint", err)
	}
	ro, err := New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://" + db, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if _, err := queryAll(t, ro, driver.Query{Text: "select count(*) from t"}); !hint(err) {
		t.Errorf("a read-only conn sharing an external-access instance = %v, want ErrReadOnly with a hint", err)
	}
	if _, err := New().Open(context.Background(), driver.ConnConfig{DSN: "duckdb://" + db + "?external_access=1", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("a read-only conn asking for external access = %v, want ErrUnsupported", err)
	}
}
