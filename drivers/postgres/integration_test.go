//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// testDSN is the DSN of the container shared by every test in the package.
var testDSN string

func TestMain(m *testing.M) { os.Exit(run(m)) }

// leakBaseline is taken after the container and seed connection exist, so
// testcontainers' and the docker client's goroutines are not counted.
var leakBaseline goleak.Option

func run(m *testing.M) int {
	ctx := context.Background()
	// PostgreSQL 17 with the pgvector extension available (not installed:
	// the shared database has no vector type; vector tests create their own).
	ctr, err := tcpostgres.Run(ctx, "pgvector/pgvector:0.8.7-pg17",
		tcpostgres.WithDatabase("tobira"), tcpostgres.WithUsername("tobira"), tcpostgres.WithPassword("tobira"),
		testcontainers.WithCmd("postgres", "-c", "max_prepared_transactions=10"),
		tcpostgres.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	testDSN, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "dsn:", err)
		return 1
	}
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	leakBaseline = goleak.IgnoreCurrent()
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(leakBaseline); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

func seed(ctx context.Context) error {
	c, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()
	stmts := drivertest.SQLSeed("postgres")
	stmts = append(stmts,
		"CREATE SCHEMA tobira_ns",
		"CREATE TABLE tobira_ns.things (id integer PRIMARY KEY, label text)",
		"INSERT INTO tobira_ns.things VALUES (1, 'in-ns')",
		"COMMENT ON SCHEMA tobira_ns IS 'a test schema'",
		"CREATE VIEW tobira_people_view AS SELECT id, name FROM tobira_people",
		"CREATE MATERIALIZED VIEW tobira_people_mv AS SELECT id FROM tobira_people",
		"CREATE TABLE tobira_orders (id integer PRIMARY KEY, person_id integer NOT NULL REFERENCES tobira_people(id), "+
			"total numeric(8,2) DEFAULT 0, doubled numeric GENERATED ALWAYS AS (total * 2) STORED, note text)",
		"CREATE INDEX tobira_orders_person_total ON tobira_orders (person_id, total)",
		"COMMENT ON TABLE tobira_orders IS 'orders table'",
		"COMMENT ON COLUMN tobira_orders.note IS 'free text'",
		"CREATE VIEW tobira_big AS SELECT g AS n FROM generate_series(1, 2000000) g",
		"CREATE TABLE tobira_series AS SELECT g AS n FROM generate_series(1, 5000) g",
		"ANALYZE tobira_people",
	)
	for _, s := range stmts {
		if _, err := c.Exec(ctx, s); err != nil {
			return fmt.Errorf("%.60s: %w", s, err)
		}
	}
	return nil
}

var scratchSeq atomic.Int64

// openPG opens a connection to the shared container and checks the pool back
// to zero acquired connections on cleanup.
func openPG(t *testing.T, readOnly bool) *conn {
	t.Helper()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN, ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	pc := c.(*conn)
	t.Cleanup(func() {
		requireIdle(t, pc)
		_ = c.Close()
	})
	return pc
}

// requireIdle fails unless every pooled connection is back. A destroyed
// connection leaves the count asynchronously, so poll briefly.
func requireIdle(t *testing.T, c *conn) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for c.pool.Stat().AcquiredConns() != 0 {
		if time.Now().After(deadline) {
			t.Errorf("%d connections still acquired", c.pool.Stat().AcquiredConns())
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// scratch copies tobira_people into a private table, so tests can write.
func scratch(t *testing.T, c *conn) string {
	t.Helper()
	name := fmt.Sprintf("tobira_scratch_%d", scratchSeq.Add(1))
	mustExec(t, c, "CREATE TABLE "+name+" AS SELECT * FROM tobira_people")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS "+name) })
	return name
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustExec(t *testing.T, c *conn, sql string) {
	t.Helper()
	if _, err := c.pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func collect(t *testing.T, c driver.Conn, q driver.Query) ([]driver.Row, driver.Stats, []driver.Column) {
	t.Helper()
	rs, err := c.Query(ctxT(t), q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatalf("Collect(%q): %v", q.Text, err)
	}
	return rows, st, cols
}

func queryErr(t *testing.T, c driver.Querier, q driver.Query) error {
	t.Helper()
	rs, err := c.Query(ctxT(t), q)
	if err != nil {
		return err
	}
	_, _, err = driver.Collect(ctxT(t), rs, 0)
	return err
}

func count(t *testing.T, c driver.Conn, table string) int64 {
	t.Helper()
	rows, _, _ := collect(t, c, driver.Query{Text: "select count(*) from " + table})
	n, _ := rows[0][0].AsInt()
	return n
}

func TestNamespaces(t *testing.T) {
	c := openPG(t, false)
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]driver.Namespace{}
	for _, n := range nss {
		by[n.Name] = n
		if n.Label != "schema" {
			t.Errorf("%s: label %q", n.Name, n.Label)
		}
	}
	if !by["public"].Default {
		t.Errorf("public is not default: %+v", by["public"])
	}
	if by["tobira_ns"].Default || by["tobira_ns"].Comment != "a test schema" {
		t.Errorf("tobira_ns = %+v", by["tobira_ns"])
	}
	for name := range by {
		if name == "pg_catalog" || name == "information_schema" || strings.HasPrefix(name, "pg_toast") || strings.HasPrefix(name, "pg_temp") {
			t.Errorf("system namespace %q listed", name)
		}
	}
}

func TestCollections(t *testing.T) {
	c := openPG(t, false)
	cols, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "public"})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]driver.Collection{}
	for _, col := range cols {
		by[col.Ref.Name] = col
		if col.Ref.Namespace != "public" {
			t.Errorf("%s: namespace %q", col.Ref.Name, col.Ref.Namespace)
		}
	}
	tests := []struct {
		name string
		typ  driver.CollectionType
	}{
		{"tobira_people", driver.CollectionTable},
		{"tobira_people_view", driver.CollectionView},
		{"tobira_people_mv", driver.CollectionMaterializedView},
	}
	for _, tt := range tests {
		if got := by[tt.name].Type; got != tt.typ {
			t.Errorf("%s type = %q, want %q", tt.name, got, tt.typ)
		}
	}
	if by["tobira_people"].RowEstimate != int64(len(drivertest.People.Rows)) {
		t.Errorf("people estimate = %d", by["tobira_people"].RowEstimate)
	}
	if by["tobira_orders"].RowEstimate != -1 {
		t.Errorf("never-analyzed estimate = %d, want -1", by["tobira_orders"].RowEstimate)
	}
	if by["tobira_orders"].Comment != "orders table" {
		t.Errorf("comment = %q", by["tobira_orders"].Comment)
	}
	if _, ok := by["tobira_people_pkey"]; ok {
		t.Error("indexes must not be listed")
	}
	other, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "tobira_ns"})
	if err != nil || len(other) != 1 || other[0].Ref.Name != "things" {
		t.Fatalf("tobira_ns = %+v, %v", other, err)
	}
}

func TestDescribePeople(t *testing.T) {
	c := openPG(t, false)
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: "public", Name: "tobira_people"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.PrimaryKey, []string{"id"}) {
		t.Errorf("pk = %v", s.PrimaryKey)
	}
	if s.Type != driver.CollectionTable || s.Ref.Namespace != "public" {
		t.Errorf("type/ref = %q %+v", s.Type, s.Ref)
	}
	f := map[string]driver.Field{}
	for _, fl := range s.Fields {
		f[fl.Name] = fl
	}
	if got := f["created_at"]; got.NativeType != "timestamp with time zone" || got.Type != driver.TypeTime || !got.Nullable {
		t.Errorf("created_at = %+v", got)
	}
	if got := f["tags"]; got.Type != driver.TypeJSON || got.NativeType != "jsonb" {
		t.Errorf("tags = %+v", got)
	}
	if got := f["name"]; got.Nullable || got.Type != driver.TypeString {
		t.Errorf("name = %+v", got)
	}
	if got := f["score"]; got.NativeType != "numeric(10,2)" || got.Type != driver.TypeDecimal {
		t.Errorf("score = %+v", got)
	}
	var unique bool
	for _, ix := range s.Indexes {
		if ix.Unique && !ix.Primary && slices.Equal(ix.Fields, []string{"email"}) && ix.Method == "btree" {
			unique = true
		}
	}
	if !unique {
		t.Errorf("no unique index on email: %+v", s.Indexes)
	}
	if s.RowEstimate != int64(len(drivertest.People.Rows)) || s.SizeBytes <= 0 {
		t.Errorf("estimate %d size %d", s.RowEstimate, s.SizeBytes)
	}
	if s.DDL != "" {
		t.Errorf("DDL = %q", s.DDL)
	}
}

func TestDescribeOrders(t *testing.T) {
	c := openPG(t, true) // catalog reads also work on a read-only conn
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_orders"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref.Namespace != "public" || s.Comment != "orders table" {
		t.Errorf("ref/comment = %+v %q", s.Ref, s.Comment)
	}
	f := map[string]driver.Field{}
	for _, fl := range s.Fields {
		f[fl.Name] = fl
	}
	if f["total"].Default != "0" || f["total"].Generated {
		t.Errorf("total = %+v", f["total"])
	}
	if !f["doubled"].Generated {
		t.Errorf("doubled = %+v", f["doubled"])
	}
	if f["note"].Comment != "free text" {
		t.Errorf("note = %+v", f["note"])
	}
	if len(s.ForeignKeys) != 1 {
		t.Fatalf("fks = %+v", s.ForeignKeys)
	}
	fk := s.ForeignKeys[0]
	if !slices.Equal(fk.Fields, []string{"person_id"}) || fk.References != (driver.CollectionRef{Namespace: "public", Name: "tobira_people"}) || !slices.Equal(fk.RefFields, []string{"id"}) {
		t.Errorf("fk = %+v", fk)
	}
	var composite bool
	for _, ix := range s.Indexes {
		if slices.Equal(ix.Fields, []string{"person_id", "total"}) {
			composite = true
		}
	}
	if !composite {
		t.Errorf("composite index missing: %+v", s.Indexes)
	}
	v, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: "public", Name: "tobira_people_view"})
	if err != nil || v.Type != driver.CollectionView || v.SizeBytes != -1 {
		t.Errorf("view = %+v, %v", v, err)
	}
}

func TestDescribeMissing(t *testing.T) {
	c := openPG(t, false)
	_, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: "public", Name: "nope"})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	_, err = c.Describe(ctxT(t), driver.CollectionRef{Namespace: "nope", Name: "tobira_people"})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestQueryColumnsAndValues(t *testing.T) {
	c := openPG(t, false)
	rows, st, cols := collect(t, c, driver.Query{Text: "select id, name, score, active, created_at, tags, age from tobira_people order by id limit 1"})
	wantTypes := []driver.Type{driver.TypeInt, driver.TypeString, driver.TypeDecimal, driver.TypeBool, driver.TypeTime, driver.TypeJSON, driver.TypeInt}
	wantNative := []string{"int4", "text", "numeric", "bool", "timestamptz", "jsonb", "int4"}
	for i, col := range cols {
		if col.Type != wantTypes[i] || col.NativeType != wantNative[i] {
			t.Errorf("col %s = %+v, want %v/%s", col.Name, col, wantTypes[i], wantNative[i])
		}
	}
	if len(rows) != 1 || st.RowsAffected != -1 {
		t.Fatalf("rows %d stats %+v", len(rows), st)
	}
	want := drivertest.People.Rows[0]
	if id, _ := rows[0][0].AsInt(); id != mustInt(want[0]) {
		t.Errorf("id = %v", rows[0][0])
	}
	if rows[0][2].String() != want[4].String() {
		t.Errorf("score = %v, want %v", rows[0][2], want[4])
	}
}

func mustInt(v driver.Value) int64 { n, _ := v.AsInt(); return n }

func TestRowsAffected(t *testing.T) {
	c := openPG(t, false)
	tbl := scratch(t, c)
	for _, q := range []driver.Query{
		{Text: "update " + tbl + " set age = 1 where id <= 3"},
		{Text: "update " + tbl + " set age = 2 where id <= 3", Limit: 5},            // tx path
		{Text: "update " + tbl + " set age = 3 where id <= 3", Namespace: "public"}, // namespace tx path
	} {
		rows, st, cols := collect(t, c, q)
		if len(rows) != 0 || len(cols) != 0 || st.RowsAffected != 3 {
			t.Errorf("%+v: rows %d cols %d stats %+v", q, len(rows), len(cols), st)
		}
	}
	if n, _ := collectInt(t, c, "select count(*) from "+tbl+" where age = 3"); n != 3 {
		t.Errorf("committed updates = %d, want 3", n)
	}
}

func collectInt(t *testing.T, c driver.Conn, sql string) (int64, bool) {
	t.Helper()
	rows, _, _ := collect(t, c, driver.Query{Text: sql})
	return rows[0][0].AsInt()
}

func TestReadOnlyHardGuard(t *testing.T) {
	c := openPG(t, true)
	total := count(t, c, "tobira_people")
	del := driver.Query{Text: "delete from tobira_people"}
	tests := []struct {
		name string
		pre  string
		// preErr: the pre statement itself must fail with ErrReadOnly.
		preErr bool
	}{
		{name: "SET default_transaction_read_only off", pre: "SET default_transaction_read_only = off"},
		{name: "set_config off", pre: "select set_config('default_transaction_read_only','off',false)"},
		{name: "SET SESSION CHARACTERISTICS read write", pre: "SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE"},
		{name: "SET TRANSACTION read write", pre: "SET TRANSACTION READ WRITE", preErr: true},
		{name: "lone COMMIT", pre: "COMMIT"},
		{name: "lone BEGIN", pre: "BEGIN"},
		{name: "BEGIN READ WRITE", pre: "BEGIN READ WRITE"},
		{name: "START TRANSACTION READ WRITE", pre: "START TRANSACTION READ WRITE"},
		{name: "ROLLBACK", pre: "ROLLBACK"},
		{name: "RESET ALL", pre: "RESET ALL"},
		{name: "PREPARE statement", pre: "PREPARE p1 AS SELECT 1"},
		{name: "advisory lock", pre: "select pg_advisory_lock(42)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := queryErr(t, c, driver.Query{Text: tt.pre})
			if tt.preErr && !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("pre: err = %v, want ErrReadOnly", err)
			}
			if err := queryErr(t, c, del); !errors.Is(err, driver.ErrReadOnly) {
				t.Fatalf("delete after %q: err = %v, want ErrReadOnly", tt.pre, err)
			}
			if got := count(t, c, "tobira_people"); got != total {
				t.Fatalf("rows = %d, want %d", got, total)
			}
		})
	}
	t.Run("compound statements are rejected", func(t *testing.T) {
		for _, text := range []string{
			"commit; delete from tobira_people",
			"begin read write; delete from tobira_people",
			"set transaction read write; delete from tobira_people",
			"select 1; delete from tobira_people",
		} {
			if err := queryErr(t, c, driver.Query{Text: text}); !errors.Is(err, driver.ErrSyntax) {
				t.Errorf("%q: err = %v, want ErrSyntax", text, err)
			}
		}
		if got := count(t, c, "tobira_people"); got != total {
			t.Fatalf("rows = %d, want %d", got, total)
		}
	})
	t.Run("writes in DO and CTE are blocked", func(t *testing.T) {
		for _, text := range []string{
			"DO $$ BEGIN SET TRANSACTION READ WRITE; DELETE FROM tobira_people; END $$",
			"with d as (delete from tobira_people returning id) select * from d",
			"create table tobira_nope (a int)",
			"truncate tobira_people",
		} {
			if err := queryErr(t, c, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%q: err = %v, want ErrReadOnly", text, err)
			}
		}
		if got := count(t, c, "tobira_people"); got != total {
			t.Fatalf("rows = %d, want %d", got, total)
		}
	})
	t.Run("PREPARE TRANSACTION is refused before it runs", func(t *testing.T) {
		for _, text := range []string{"PREPARE TRANSACTION 'px'", "/* x */ prepare transaction 'px'"} {
			if err := queryErr(t, c, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%q: err = %v, want ErrReadOnly", text, err)
			}
		}
		if n, _ := collectInt(t, openPG(t, false), "select count(*) from pg_prepared_xacts"); n != 0 {
			t.Errorf("%d prepared transactions left on the server", n)
		}
	})
	t.Run("procedure and DO block that COMMIT", func(t *testing.T) {
		w := openPG(t, false)
		mustExec(t, w, "CREATE OR REPLACE PROCEDURE tobira_wipe() LANGUAGE plpgsql AS $$ BEGIN DELETE FROM tobira_people; COMMIT; END $$")
		defer mustExec(t, w, "DROP PROCEDURE tobira_wipe()")
		for _, text := range []string{
			"CALL tobira_wipe()",
			"DO $$ BEGIN DELETE FROM tobira_people; COMMIT; END $$",
		} {
			if err := queryErr(t, c, driver.Query{Text: text}); err == nil {
				t.Errorf("%q: want an error", text)
			}
		}
		if got := count(t, c, "tobira_people"); got != total {
			t.Fatalf("rows = %d, want %d", got, total)
		}
	})
	t.Run("with Limit", func(t *testing.T) {
		q := driver.Query{Text: "with d as (delete from tobira_people returning id) select * from d", Limit: 5}
		if err := queryErr(t, c, q); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("err = %v, want ErrReadOnly", err)
		}
		if got := count(t, c, "tobira_people"); got != total {
			t.Fatalf("rows = %d, want %d", got, total)
		}
	})
}

func TestPerQueryReadOnlyOnWritableConn(t *testing.T) {
	c := openPG(t, false)
	tbl := scratch(t, c)
	total := count(t, c, tbl)
	del := driver.Query{Text: "delete from " + tbl + " where id = 1", ReadOnly: true}
	if err := queryErr(t, c, del); !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("err = %v, want ErrReadOnly", err)
	}
	if got := count(t, c, tbl); got != total {
		t.Fatalf("rows = %d, want %d", got, total)
	}
	del.ReadOnly = false
	rows, st, _ := collect(t, c, del)
	if len(rows) != 0 || st.RowsAffected != 1 {
		t.Fatalf("rows %d stats %+v", len(rows), st)
	}
	if got := count(t, c, tbl); got != total-1 {
		t.Fatalf("rows = %d, want %d", got, total-1)
	}
	// a ReadOnly query must not leave the pooled connection read-only
	if _, st, _ := collect(t, c, driver.Query{Text: "update " + tbl + " set age = 9"}); st.RowsAffected != total-1 {
		t.Errorf("update stats %+v", st)
	}
}

func TestLimitDoesNotDrain(t *testing.T) {
	for _, tt := range []struct {
		name     string
		readOnly bool
	}{{"read-write", false}, {"read-only", true}} {
		t.Run(tt.name, func(t *testing.T) {
			c := openPG(t, tt.readOnly)
			start := time.Now()
			rs, err := c.Query(ctxT(t), driver.Query{Text: "select n from tobira_big", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			rows, st, err := driver.Collect(ctxT(t), rs, 0)
			if err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("took %v, want < 1s", d)
			}
			if len(rows) != 10 || !st.Truncated {
				t.Errorf("rows %d truncated %v", len(rows), st.Truncated)
			}
			if n, _ := rows[9][0].AsInt(); n != 10 {
				t.Errorf("last row = %v", rows[9][0])
			}
		})
	}
}

func TestLimitBoundaries(t *testing.T) {
	c := openPG(t, false)
	tests := []struct {
		name      string
		text      string
		limit     int
		wantRows  int
		wantTrunc bool
	}{
		{"fewer than limit", "select n from tobira_series where n <= 7", 10, 7, false},
		{"exactly limit", "select n from tobira_series where n <= 10", 10, 10, false},
		{"one over limit", "select n from tobira_series where n <= 11", 10, 10, true},
		{"limit 1", "select n from tobira_series", 1, 1, true},
		{"spans fetch batches", "select n from tobira_series", 1200, 1200, true},
		{"exactly batch size", "select n from tobira_series where n <= 500", 500, 500, false},
		{"empty result", "select n from tobira_series where n < 0", 5, 0, false},
		{"values", "values (1),(2),(3)", 2, 2, true},
		{"table", "table tobira_series", 3, 3, true},
		{"leading comment and parens", "/* c */ -- x\n (select n from tobira_series order by n)", 3, 3, true},
		{"trailing semicolon", "select n from tobira_series;", 3, 3, true},
		{"non-cursor statement (show)", "show all", 3, 3, true},
		{"explain", "explain select * from tobira_series a join tobira_series b using (n)", 1, 1, true},
	}
	for _, ro := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("ro=%v/%s", ro, tt.name), func(t *testing.T) {
				cc := c
				if ro {
					cc = openPG(t, true)
				}
				rows, st, _ := collect(t, cc, driver.Query{Text: tt.text, Limit: tt.limit})
				if len(rows) != tt.wantRows || st.Truncated != tt.wantTrunc {
					t.Errorf("rows %d truncated %v, want %d %v", len(rows), st.Truncated, tt.wantRows, tt.wantTrunc)
				}
			})
		}
	}
}

func TestLimitedDataModifyingCTE(t *testing.T) {
	c := openPG(t, false)
	tbl := scratch(t, c)
	total := count(t, c, tbl)
	q := driver.Query{Text: "with d as (delete from " + tbl + " where id <= 5 returning id) select id from d order by id", Limit: 3}
	rows, st, _ := collect(t, c, q)
	if len(rows) != 3 || !st.Truncated {
		t.Fatalf("rows %d stats %+v", len(rows), st)
	}
	if got := count(t, c, tbl); got != total-5 {
		t.Errorf("rows = %d, want %d (delete must be committed)", got, total-5)
	}
}

func TestMultipleStatementsRejected(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openPG(t, ro)
		tbl := "tobira_people"
		if !ro {
			tbl = scratch(t, c)
		}
		before := count(t, c, tbl)
		tests := []struct {
			name string
			q    driver.Query
		}{
			{"no params", driver.Query{Text: "select 1; delete from " + tbl}},
			{"with params", driver.Query{Text: "select $1::int; delete from " + tbl, Params: []driver.Value{driver.Int(1)}}},
			{"with limit", driver.Query{Text: "select 1; delete from " + tbl, Limit: 5}},
			{"with limit and params", driver.Query{Text: "select $1::int; delete from " + tbl, Params: []driver.Value{driver.Int(1)}, Limit: 5}},
			{"with namespace", driver.Query{Text: "select 1; delete from " + tbl, Namespace: "public"}},
			{"with CTE and limit", driver.Query{Text: "with x as (select 1) select * from x; delete from " + tbl, Limit: 5}},
			{"two selects", driver.Query{Text: "select 1; select 2"}},
		}
		for _, tt := range tests {
			t.Run(fmt.Sprintf("ro=%v/%s", ro, tt.name), func(t *testing.T) {
				err := queryErr(t, c, tt.q)
				if !errors.Is(err, driver.ErrSyntax) {
					t.Fatalf("err = %v, want ErrSyntax", err)
				}
				if got := count(t, c, tbl); got != before {
					t.Fatalf("rows = %d, want %d", got, before)
				}
			})
		}
	}
}

func TestMultipleStatementsRejectedErrorCode(t *testing.T) {
	c := openPG(t, false)
	err := queryErr(t, c, driver.Query{Text: "select 1; select 2"})
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != "42601" || de.Driver != Name {
		t.Fatalf("err = %#v", err)
	}
}

func TestCursorRespectsParams(t *testing.T) {
	for _, ro := range []bool{false, true} {
		t.Run(fmt.Sprintf("ro=%v", ro), func(t *testing.T) {
			c := openPG(t, ro)
			q := driver.Query{
				Text:   "select n, $2::text as tag from tobira_series where n > $1 order by n",
				Params: []driver.Value{driver.Int(100), driver.String("x")}, Limit: 4,
			}
			rows, st, cols := collect(t, c, q)
			if len(rows) != 4 || !st.Truncated {
				t.Fatalf("rows %d truncated %v", len(rows), st.Truncated)
			}
			if n, _ := rows[0][0].AsInt(); n != 101 {
				t.Errorf("first = %v, want 101", rows[0][0])
			}
			if s, _ := rows[3][1].AsText(); s != "x" || cols[1].Name != "tag" {
				t.Errorf("tag = %v col %+v", rows[3][1], cols[1])
			}
		})
	}
}

func TestParamTypes(t *testing.T) {
	c := openPG(t, false)
	ts := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	q := driver.Query{
		Text:   "select $1::bool, $2::int8, $3::float8, $4::numeric, $5::text, $6::timestamptz, $7::jsonb, $8::bytea, $9::int[]",
		Params: []driver.Value{driver.Bool(true), driver.Int(7), driver.Float(1.5), driver.Decimal("12.50"), driver.String("hi"), driver.Time(ts), driver.JSON([]byte(`{"a":1}`)), driver.Bytes([]byte{1, 2}), driver.List(driver.Int(1), driver.Int(2))},
	}
	rows, _, _ := collect(t, c, q)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if got, _ := rows[0][3].AsText(); got != "12.50" {
		t.Errorf("decimal = %q", got)
	}
	if got, _ := rows[0][5].AsTime(); !got.Equal(ts) {
		t.Errorf("time = %v", got)
	}
	if l, _ := rows[0][8].AsList(); len(l) != 2 {
		t.Errorf("list = %v", rows[0][8])
	}
}

func TestNamespaceSearchPath(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openPG(t, ro)
		rows, _, _ := collect(t, c, driver.Query{Text: "select label from things", Namespace: "tobira_ns"})
		if len(rows) != 1 {
			t.Fatalf("ro=%v rows = %d", ro, len(rows))
		}
		if s, _ := rows[0][0].AsText(); s != "in-ns" {
			t.Errorf("label = %q", s)
		}
		// the search_path must not leak to the next call on a pooled conn
		if err := queryErr(t, c, driver.Query{Text: "select * from things"}); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("ro=%v: leaked search_path: err = %v", ro, err)
		}
		// quoting: a hostile namespace is just an unknown schema
		if err := queryErr(t, c, driver.Query{Text: "select 1", Namespace: `x"; drop table tobira_people; --`}); err != nil {
			t.Errorf("hostile namespace: %v", err)
		}
		if count(t, c, "tobira_people") == 0 {
			t.Fatal("people table was dropped")
		}
	}
}

func TestQueryErrorMapping(t *testing.T) {
	c := openPG(t, false)
	tests := []struct {
		name string
		q    driver.Query
		want error
	}{
		{"missing table", driver.Query{Text: "select * from nope"}, driver.ErrNotFound},
		{"missing table with limit", driver.Query{Text: "select * from nope", Limit: 3}, driver.ErrNotFound},
		{"syntax", driver.Query{Text: "selec 1"}, driver.ErrSyntax},
		{"syntax with limit", driver.Query{Text: "selec 1", Limit: 3}, driver.ErrSyntax},
		{"with-syntax with limit", driver.Query{Text: "with x as (selec 1) select * from x", Limit: 3}, driver.ErrSyntax},
		{"constraint", driver.Query{Text: "insert into tobira_people (id, name, email) values (1, 'a', 'b')"}, driver.ErrConstraint},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := queryErr(t, c, tt.q); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCancelMidQuery(t *testing.T) {
	for _, tt := range []struct {
		name string
		q    driver.Query
		ro   bool
	}{
		{"plain", driver.Query{Text: "select pg_sleep(10)"}, false},
		{"read-only tx", driver.Query{Text: "select pg_sleep(10)"}, true},
		{"limited", driver.Query{Text: "select pg_sleep(10)", Limit: 5}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := openPG(t, tt.ro)
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(100*time.Millisecond, cancel)
			start := time.Now()
			rs, err := c.Query(ctx, tt.q)
			if err == nil {
				_, _, err = driver.Collect(ctx, rs, 0)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("took %v, want < 2s", d)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if rows, _, _ := collect(t, c, driver.Query{Text: "select 42"}); len(rows) != 1 {
				t.Errorf("pool unusable after cancel")
			}
		})
	}
}

func TestNoConnLeak(t *testing.T) {
	// 20 rounds: each runs a few dozen queries, so a leak of even one
	// connection per round would exhaust the default pool long before the end.
	c := openPG(t, false)
	ro := openPG(t, true)
	tbl := scratch(t, c)
	for i := range 20 {
		for _, cc := range []*conn{c, ro} {
			// exhausted
			collect(t, cc, driver.Query{Text: "select n from tobira_series", Limit: 5})
			collect(t, cc, driver.Query{Text: "select n from tobira_series where n < 3"})
			collect(t, cc, driver.Query{Text: "select 1", ReadOnly: true})
			// abandoned and closed early, never read
			for _, q := range []driver.Query{
				{Text: "select n from tobira_series", Limit: 100},
				{Text: "select n from tobira_series"},
				{Text: "select n from tobira_series", ReadOnly: true},
				{Text: "show all", Limit: 2},
				{Text: "select n from tobira_series", Namespace: "public"},
			} {
				rs, err := cc.Query(ctxT(t), q)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := rs.Next(ctxT(t)); err != nil {
					t.Fatalf("%q ro=%v: %v", q.Text, cc.readOnly, err)
				}
				if err := rs.Close(); err != nil {
					t.Fatal(err)
				}
				if err := rs.Close(); err != nil { // idempotent
					t.Fatal(err)
				}
			}
			// errors
			_ = queryErr(t, cc, driver.Query{Text: "select * from nope", Limit: 3})
			_ = queryErr(t, cc, driver.Query{Text: "select 1; select 2", ReadOnly: true})
			_ = queryErr(t, cc, driver.Query{Text: "delete from " + tbl, ReadOnly: true})
			// cancelled before and during
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := cc.Query(ctx, driver.Query{Text: "select 1", Limit: 1}); err == nil {
				t.Fatal("cancelled ctx: want error")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
			rs, err := cc.Query(ctx, driver.Query{Text: "select pg_sleep(5)", Limit: 1})
			if err == nil {
				_, _, err = driver.Collect(ctx, rs, 0)
			}
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("iter %d: err = %v, want deadline", i, err)
			}
			// a ResultSet whose Next ctx is cancelled mid-stream
			rs, err = cc.Query(ctxT(t), driver.Query{Text: "select n from tobira_series", Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			cctx, ccancel := context.WithCancel(context.Background())
			ccancel()
			if _, _, err := rs.Next(cctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Next on cancelled ctx: %v", err)
			}
			_ = rs.Close()
		}
	}
	for _, cc := range []*conn{c, ro} {
		requireIdle(t, cc)
	}
	// the catalog paths also release
	for range 5 {
		_, _ = c.Namespaces(ctxT(t))
		_, _ = c.Collections(ctxT(t), driver.NamespaceRef{Name: "public"})
		_, _ = c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_people"})
		_, _ = c.Describe(ctxT(t), driver.CollectionRef{Name: "nope"})
	}
	requireIdle(t, c)
}

func TestTypeLookupBoundedAcquire(t *testing.T) {
	// With the pool's only connection taken, the type lookup must give up
	// after typeLookupTimeout instead of waiting for the caller's deadline.
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN + "&pool_max_conns=1"})
	if err != nil {
		t.Fatal(err)
	}
	pc := c.(*conn)
	t.Cleanup(func() { _ = c.Close() })
	held, err := pc.pool.Acquire(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	pc.lookupTypeNames(ctxT(t), []int64{1})
	elapsed := time.Since(start)
	held.Release()
	requireIdle(t, pc)
	if elapsed < typeLookupTimeout/2 || elapsed > 3*typeLookupTimeout {
		t.Errorf("lookup returned after %v, want about %v", elapsed, typeLookupTimeout)
	}
	if _, ok := pc.typeNames.Load(uint32(1)); ok {
		t.Error("a lookup that never got a connection stored a name")
	}
}

func TestSinglePoolConnection(t *testing.T) {
	// A one-connection pool must not deadlock on type lookups or cursors.
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN + "&pool_max_conns=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	mustExec(t, c.(*conn), "CREATE TYPE tobira_mood AS ENUM ('sad','ok')")
	defer mustExec(t, c.(*conn), "DROP TYPE tobira_mood")
	rows, _, cols := collect(t, c, driver.Query{Text: "select 'ok'::tobira_mood as m"})
	if len(rows) != 1 || cols[0].Type != driver.TypeUnknown {
		t.Fatalf("rows %v cols %+v", rows, cols)
	}
	collect(t, c, driver.Query{Text: "select n from tobira_series", Limit: 3})
}

func TestCustomTypeNames(t *testing.T) {
	c := openPG(t, false)
	mustExec(t, c, "CREATE TYPE tobira_mood2 AS ENUM ('sad','ok')")
	defer mustExec(t, c, "DROP TYPE tobira_mood2")
	rows, _, cols := collect(t, c, driver.Query{Text: "select 'ok'::tobira_mood2 as m, interval '1 day' as i, 1 as n"})
	if cols[0].NativeType != "tobira_mood2" || cols[0].Type != driver.TypeUnknown {
		t.Errorf("enum col = %+v", cols[0])
	}
	if cols[1].NativeType != "interval" || cols[1].Type != driver.TypeUnknown {
		t.Errorf("interval col = %+v", cols[1])
	}
	if s := rows[0][0].String(); s != "ok" {
		t.Errorf("enum value = %q", s)
	}
}

func TestSessionStateDoesNotSurviveCalls(t *testing.T) {
	other := openPG(t, false)
	for _, ro := range []bool{true, false} {
		c := openPG(t, ro)
		if err := queryErr(t, c, driver.Query{Text: "select pg_advisory_lock(4242)"}); err != nil {
			t.Fatal(err)
		}
		// the lock must be gone once the call is over: a different session can take it
		deadline := time.Now().Add(3 * time.Second)
		for {
			if n, _ := collectInt(t, other, "select case when pg_try_advisory_lock(4242) then 1 else 0 end"); n == 1 {
				mustExec(t, other, "select pg_advisory_unlock_all()")
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("ro=%v: advisory lock outlived the call", ro)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := queryErr(t, c, driver.Query{Text: "PREPARE tobira_p1 AS SELECT 1"}); err != nil {
			t.Fatal(err)
		}
		for range 10 { // whichever pooled connection serves the next call
			if n, _ := collectInt(t, c, "select count(*) from pg_prepared_statements"); n != 0 {
				t.Fatalf("ro=%v: prepared statement survived: %d", ro, n)
			}
		}
	}
}

func TestWritableConnSettingsDoNotLeak(t *testing.T) {
	c := openPG(t, false)
	for _, set := range []string{"SET search_path = tobira_ns", "SET ROLE NONE", "SET statement_timeout = 1"} {
		if err := queryErr(t, c, driver.Query{Text: set}); err != nil {
			t.Fatal(err)
		}
	}
	for range 10 {
		if err := queryErr(t, c, driver.Query{Text: "select * from things"}); !errors.Is(err, driver.ErrNotFound) {
			t.Fatalf("search_path leaked: %v", err)
		}
		if rows, _, _ := collect(t, c, driver.Query{Text: "show statement_timeout"}); rows[0][0].String() != "0" {
			t.Fatalf("statement_timeout leaked: %v", rows[0][0])
		}
	}
}

func TestReadOnlyParamSurvivesReset(t *testing.T) {
	c := openPG(t, true)
	for range 6 {
		collect(t, c, driver.Query{Text: "select 1"})
		if err := c.pool.QueryRow(context.Background(), "show default_transaction_read_only").Scan(new(string)); err != nil {
			t.Fatal(err)
		}
	}
	var v string
	if err := c.pool.QueryRow(context.Background(), "show default_transaction_read_only").Scan(&v); err != nil || v != "on" {
		t.Fatalf("default_transaction_read_only = %q, %v", v, err)
	}
}

func TestTypeLookupOnSaturatedPool(t *testing.T) {
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN + "&pool_max_conns=2"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	mustExec(t, c.(*conn), "CREATE TYPE tobira_mood3 AS ENUM ('a','b')")
	defer mustExec(t, c.(*conn), "DROP TYPE tobira_mood3")
	ctx := ctxT(t)
	held, err := c.Query(ctx, driver.Query{Text: "select n from tobira_series"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if _, _, err := held.Next(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rs, err := c.Query(qctx, driver.Query{Text: "select 'a'::tobira_mood3 as m"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(qctx, rs, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v err %v", rows, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v on a saturated pool", d)
	}
}

func TestCancelKeepsConnection(t *testing.T) {
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN + "&pool_max_conns=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	pid := func() int64 { n, _ := collectInt(t, c, "select pg_backend_pid()"); return n }
	before := pid()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := queryErr2(ctx, c, driver.Query{Text: "select pg_sleep(10)"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if after := pid(); after != before {
		t.Errorf("cancel replaced the backend (%d -> %d)", before, after)
	}
}

func queryErr2(ctx context.Context, c driver.Conn, q driver.Query) error {
	rs, err := c.Query(ctx, q)
	if err != nil {
		return err
	}
	_, _, err = driver.Collect(ctx, rs, 0)
	return err
}
