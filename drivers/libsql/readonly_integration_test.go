//go:build integration

package libsql

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// sqldRefuses are reads the guard allows that sqld's own parser refuses: a
// leading byte order mark and a quoted pragma name.
var sqldRefuses = map[string]bool{
	"\xEF\xBB\xBFSELECT 1":                    true,
	`PRAGMA "table_info"(tobira_people)`:      true,
	`PRAGMA main."table_info"(tobira_people)`: true,
}

func userVersion(t *testing.T, c driver.Querier) int64 {
	t.Helper()
	rows, _, _ := run(t, c, driver.Query{Text: "PRAGMA user_version", ReadOnly: true})
	n, _ := rows[0][0].AsInt()
	return n
}

func tableExists(t *testing.T, c driver.Querier, name string) bool {
	t.Helper()
	rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from sqlite_schema where name = ?", Params: []driver.Value{driver.String(name)}, ReadOnly: true})
	n, _ := rows[0][0].AsInt()
	return n > 0
}

// Every bypass the unit tests list is refused by sqld's driver too, on a
// read-only connection and with Query.ReadOnly, and nothing changes.
func TestReadOnlyGuardOnServer(t *testing.T) {
	for _, mode := range []string{"ro conn", "Query.ReadOnly"} {
		t.Run(mode, func(t *testing.T) {
			c := openLibsql(t, mode == "ro conn")
			rw := openLibsql(t, false)
			ro := mode == "Query.ReadOnly"
			ctx := context.Background()
			beforeCount, beforeVersion := count(t, rw, "tobira_people"), userVersion(t, rw)
			try := func(text string) error {
				rs, err := c.Query(ctx, driver.Query{Text: text, ReadOnly: ro})
				if err == nil {
					_, _, err = driver.Collect(ctx, rs, 0)
				}
				return err
			}
			for _, text := range readOnlyDenied {
				if err := try(text); !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("%q: got %v, want ErrReadOnly", text, err)
				}
			}
			for _, text := range readOnlyAllowed {
				err := try(text)
				if sqldRefuses[text] {
					// The guard allows it; sqld's own parser refuses it.
					if err == nil {
						t.Errorf("%q: sqld accepted it; drop it from sqldRefuses", text)
					}
					continue
				}
				if err != nil {
					t.Errorf("%q: %v", text, err)
				}
			}
			if n := count(t, rw, "tobira_people"); n != beforeCount {
				t.Errorf("row count %d -> %d", beforeCount, n)
			}
			if v := userVersion(t, rw); v != beforeVersion {
				t.Errorf("user_version %d -> %d", beforeVersion, v)
			}
			for _, name := range []string{"tt"} {
				if tableExists(t, rw, name) {
					t.Errorf("table %s was created", name)
				}
			}
			// Parameters still work on the read-only path.
			rows, _, _ := run(t, c, driver.Query{Text: "select name from tobira_people where id = ?", Params: []driver.Value{driver.Int(1)}, ReadOnly: ro})
			if len(rows) != 1 {
				t.Errorf("parameterised select rows = %v", rows)
			}
		})
	}
}

// The second layer: a write that got past the lexical gate still runs inside
// BEGIN … ROLLBACK, so the server undoes it.
func TestReadOnlyPathRollsBack(t *testing.T) {
	c := openLibsql(t, true)
	rw := openLibsql(t, false)
	ctx := context.Background()
	table := scratch(t, rw)
	before := count(t, rw, table)
	for _, text := range []string{
		"insert into " + table + " (id, name, email) values (5000, 'x', 'x5000@x')",
		"delete from " + table,
		"update " + table + " set name = 'pwned'",
		"with c as (select 1) delete from " + table,
		"create table tobira_bypass (a)",
		"pragma user_version = 99",
	} {
		r, err := c.readOnlyExec(ctx, hranaStmt{SQL: text})
		if err != nil {
			t.Logf("%q refused by the server: %v", text, err)
			continue
		}
		_ = r
	}
	if n := count(t, rw, table); n != before {
		t.Errorf("row count %d -> %d: a write escaped the rollback", before, n)
	}
	rows, _, _ := run(t, rw, driver.Query{Text: "select count(*) from " + table + " where name = 'pwned'"})
	if n, _ := rows[0][0].AsInt(); n != 0 {
		t.Errorf("%d rows renamed", n)
	}
	if tableExists(t, rw, "tobira_bypass") {
		t.Error("a CREATE TABLE escaped the rollback")
	}
	if v := userVersion(t, rw); v == 99 {
		t.Error("a PRAGMA assignment escaped the rollback")
	}
}

// A Turso/sqld read-only token is enforced by the server: reads work, writes
// fail with ErrPermission, on any connection.
func TestReadOnlyToken(t *testing.T) {
	ctx := context.Background()
	c, err := New().Open(ctx, cfgFor(false, roToken))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if n := count(t, c, "tobira_people"); n != int64(len(drivertest.People.Rows)) {
		t.Errorf("count = %d", n)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "delete from tobira_people"}); !errors.Is(err, driver.ErrPermission) {
		t.Errorf("write with a read-only token: %v, want ErrPermission", err)
	}
	m, _ := driver.As[driver.Mutator](c)
	if _, err := m.Insert(ctx, driver.CollectionRef{Namespace: "main", Name: "tobira_people"},
		driver.Object{{Key: "id", Value: driver.Int(9000)}, {Key: "name", Value: driver.String("n")}, {Key: "email", Value: driver.String("n@x")}}); !errors.Is(err, driver.ErrPermission) {
		t.Errorf("insert with a read-only token: %v, want ErrPermission", err)
	}
	if _, err := c.(*conn).Describe(ctx, driver.CollectionRef{Name: "tobira_people"}); err != nil {
		t.Errorf("describe with a read-only token: %v", err)
	}
}

func TestReadOnlyConnRefusesMutationsAndRWTx(t *testing.T) {
	c := openLibsql(t, true)
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(9001)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("insert: %v", err)
	}
	if err := c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("update: %v", err)
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("delete: %v", err)
	}
	if _, err := c.Begin(ctx, driver.TxOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-write begin: %v", err)
	}
	if _, err := c.Explain(ctx, driver.Query{Text: "PRAGMA user_version = 5"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("explain of a pragma assignment: %v", err)
	}
}

func TestReadOnlyTx(t *testing.T) {
	c := openLibsql(t, false)
	ctx := context.Background()
	table := scratch(t, c)
	tx, err := c.Begin(ctx, driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if n := count(t, tx, table); n != int64(len(drivertest.People.Rows)) {
		t.Errorf("count in tx = %d", n)
	}
	for _, text := range []string{"delete from " + table, "with c as (select 1) delete from " + table, "commit", "PRAGMA user_version=3"} {
		if _, err := tx.Query(ctx, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q in a read-only tx: %v", text, err)
		}
	}
	m := tx.(driver.Mutator)
	if err := m.Delete(ctx, driver.CollectionRef{Name: table}, driver.Object{{Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("delete in a read-only tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, table); n != int64(len(drivertest.People.Rows)) {
		t.Errorf("count after = %d", n)
	}
}

// A statement that ends the transaction behind the driver's back (sent
// below the keyword refusal) leaves the Tx dead: nothing more runs on it.
func TestTxEscapeFailsClosed(t *testing.T) {
	for _, ro := range []bool{false, true} {
		t.Run(fmt.Sprint("readOnly=", ro), func(t *testing.T) {
			c := openLibsql(t, false)
			ctx := context.Background()
			table := scratch(t, c)
			dtx, err := c.Begin(ctx, driver.TxOptions{ReadOnly: ro})
			if err != nil {
				t.Fatal(err)
			}
			lt := dtx.(*tx)
			if _, err := lt.s.send(ctx, executeReq(hranaStmt{SQL: "COMMIT"})); err != nil {
				t.Fatal(err)
			}
			if _, err := dtx.Query(ctx, driver.Query{Text: "select 1"}); err == nil {
				t.Error("a statement ran after the Tx escaped")
			}
			if _, err := dtx.Query(ctx, driver.Query{Text: "update " + table + " set name = 'pwned'"}); err == nil {
				t.Error("a write ran after the Tx escaped")
			}
			if err := dtx.Commit(ctx); err == nil {
				t.Error("Commit must fail after the Tx escaped")
			}
			_ = dtx.Rollback(ctx)
			rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from " + table + " where name = 'pwned'"})
			if n, _ := rows[0][0].AsInt(); n != 0 {
				t.Error("write persisted")
			}
		})
	}
}
