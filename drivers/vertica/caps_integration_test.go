//go:build integration

package vertica

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// pinned is one session of the admin pool, outside the driver's guard.
func pinned(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	db := adminDB()
	t.Cleanup(func() { _ = db.Close() })
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return db, c
}

func scalarText(t *testing.T, c *sql.Conn, text string) (string, error) {
	t.Helper()
	var s sql.NullString
	err := c.QueryRowContext(noStdin(context.Background()), text).Scan(&s)
	return s.String, err
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(noStdin(context.Background()), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestServerLexingProbes pins the server behaviour the guard's choices
// rest on, straight against Vertica with no guard in the way.
func TestServerLexingProbes(t *testing.T) {
	_, c := pinned(t)
	for text, want := range map[string]string{
		"SELECT 1 -- c\r+1":          "2",  // a lone CR ends a -- comment: the guard refuses it
		"SELECT /* a /* b */ 1 */ 2": "2",  // block comments nest: the guard refuses nesting
		"SELECT 0x1F":                "31", // hex literals: the lexer reads 0x… as one number
		"SELECT E'a\\'b'":            "a'b",
		"SELECT $q$x;y$q$":           "x;y",
	} {
		got, err := scalarText(t, c, text)
		if err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", text, got, err, want)
		}
	}
	// The server's prepared-statement Parse refuses a second statement
	// (the guard and checkShape refuse it before it gets there).
	_, err := scalarText(t, c, "SELECT 1 AS begin; SELECT 2")
	if err == nil || !strings.Contains(err.Error(), "multiple commands") {
		t.Errorf("two statements in one Parse = %v, want Vertica's refusal", err)
	}
}

// TestAutocommitIgnoresStartTransaction pins why transactions turn
// autocommit off: under autocommit, START TRANSACTION READ ONLY does not
// stop a write.
func TestAutocommitIgnoresStartTransaction(t *testing.T) {
	db, c := pinned(t)
	ctx := noStdin(context.Background())
	mustExec(t, db, "CREATE TABLE IF NOT EXISTS "+itSchema+".ac_probe (a INT)")
	before := countRows(t, db, itSchema+".ac_probe")
	for _, s := range []string{"START TRANSACTION READ ONLY", "INSERT INTO " + itSchema + ".ac_probe VALUES (1)", "ROLLBACK"} {
		if _, err := c.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if after := countRows(t, db, itSchema+".ac_probe"); after != before+1 {
		t.Errorf("under autocommit the write inside START TRANSACTION READ ONLY: %d -> %d rows, want it committed", before, after)
	}
}

// TestMetaFunctionsIgnoreReadOnlyTransactions pins why the guard
// allow-lists functions: a meta-function acts inside a READ ONLY
// transaction.
func TestMetaFunctionsIgnoreReadOnlyTransactions(t *testing.T) {
	_, c := pinned(t)
	ctx := noStdin(context.Background())
	for _, s := range []string{"SET SESSION AUTOCOMMIT TO OFF", "START TRANSACTION READ ONLY"} {
		if _, err := c.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := scalarText(t, c, "SELECT SET_CONFIG_PARAMETER('MaxClientSessions', 50)")
	_, _ = c.ExecContext(ctx, "ROLLBACK")
	_, _ = c.ExecContext(ctx, "SET SESSION AUTOCOMMIT TO ON")
	if err != nil || !strings.Contains(strings.ToLower(got), "success") {
		t.Errorf("SET_CONFIG_PARAMETER in a READ ONLY transaction = %q, %v; want it to act", got, err)
	}
}

// Every bypass the guard refuses is refused by the driver against the real
// server, on a read-only connection and with Query.ReadOnly, and nothing
// changes.
func TestReadOnlyGuardOnServer(t *testing.T) {
	db := adminDB()
	defer func() { _ = db.Close() }()
	ro := openTest(t, true)
	rw := openTest(t, false)
	before := countRows(t, db, "public.tobira_people")
	for _, text := range []string{
		"DELETE FROM tobira_people",
		"select 1; delete from tobira_people",
		"select begin from tobira_people; delete from tobira_people",
		"WITH c AS (SELECT 1) DELETE FROM tobira_people",
		"SELECT * INTO tobira_people_copy FROM tobira_people",
		"SELECT * FROM tobira_people FOR UPDATE",
		"SELECT 0x1delete from tobira_people",
		"SELECT 1 -- c\rdelete from tobira_people",
		"SELECT /* a /* b */ 1 */ 2",
		"EXPLAIN ANALYZE DELETE FROM tobira_people",
		"SELECT SET_CONFIG_PARAMETER('MaxClientSessions', 0)",
		"SELECT CLOSE_ALL_SESSIONS()",
		"SELECT EXPORT_TABLES('', 'tobira_people')",
		`SELECT "SHUTDOWN"()`,
		"SELECT public.anything(1)",
		"SELECT malé(1)",
		"SELECT cube(1)",
		"COPY tobira_people FROM STDIN",
		"EXPORT TO PARQUET(directory = '/tmp/x') AS SELECT * FROM tobira_people",
		"CONNECT TO VERTICA tobira USER dbadmin PASSWORD 'x' ON 'localhost', 5433",
		"SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE",
		`SELECT '\'' ; DELETE FROM tobira_people --'`,
	} {
		_, err := ro.Query(ctxT(t), driver.Query{Text: text})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only conn: Query(%q) = %v, want ErrReadOnly", text, err)
		}
		_, err = rw.Query(ctxT(t), driver.Query{Text: text, ReadOnly: true})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Query.ReadOnly: Query(%q) = %v, want ErrReadOnly", text, err)
		}
	}
	if after := countRows(t, db, "public.tobira_people"); after != before {
		t.Errorf("tobira_people: %d rows before, %d after", before, after)
	}
	_, c := pinned(t)
	if got, _ := scalarText(t, c, "SELECT GET_CONFIG_PARAMETER('MaxClientSessions')"); got == "0" {
		t.Error("MaxClientSessions was changed")
	}
}

// The server-side layer holds on its own: a write that reaches a read-only
// session (or a READ ONLY transaction) fails with ErrReadOnly.
func TestReadOnlyServerLayer(t *testing.T) {
	db := adminDB()
	defer func() { _ = db.Close() }()
	write := "INSERT INTO " + itSchema + ".things VALUES (99, 'must-not-land')"
	before := countRows(t, db, itSchema+".things")

	ro := openTest(t, true)
	s, err := ro.acquire(ctxT(t), "test", ro.db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.exec(ctxT(t), write)
	s.release()
	if err = mapError(context.Background(), "query", err); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("write on a read-only connection's session = %v, want ErrReadOnly", err)
	}

	rw := openTest(t, false)
	s, err = rw.acquire(ctxT(t), "test", rw.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.begin(ctxT(t), true, ""); err != nil {
		t.Fatal(err)
	}
	_, err = s.exec(ctxT(t), write)
	_ = s.end(false)
	s.release()
	if err = mapError(context.Background(), "query", err); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("write in the driver's READ ONLY transaction = %v, want ErrReadOnly", err)
	}
	if after := countRows(t, db, itSchema+".things"); after != before {
		t.Errorf("things: %d rows before, %d after", before, after)
	}

	// After a read-only call the session is back in autocommit: a write on
	// the same pool commits.
	if _, err := rw.Query(ctxT(t), driver.Query{Text: "SELECT 1", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	rs, err := rw.Query(ctxT(t), driver.Query{Text: "INSERT INTO " + itSchema + ".things VALUES (50, 'committed')"})
	if err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	if after := countRows(t, db, itSchema+".things"); after != before+1 {
		t.Errorf("a write after read-only calls: %d rows, want %d (committed)", after, before+1)
	}
	mustExec(t, db, "DELETE FROM "+itSchema+".things WHERE id = 50")
}

// COPY from the client is refused on every path; a COPY FROM STDIN that
// reached the SDK anyway would read nothing, never the process's stdin.
func TestClientCopyRefused(t *testing.T) {
	db := adminDB()
	defer func() { _ = db.Close() }()
	rw := openTest(t, false)
	file := filepath.Join(t.TempDir(), "secret.csv")
	if err := os.WriteFile(file, []byte("77|from-a-local-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, db, itSchema+".things")
	for _, text := range []string{
		"COPY " + itSchema + ".things FROM LOCAL '" + file + "' DELIMITER '|'",
		"COPY " + itSchema + ".things FROM STDIN DELIMITER '|'",
	} {
		if _, err := rw.Query(ctxT(t), driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Query(%q) = %v, want ErrUnsupported", text, err)
		}
	}
	s, err := rw.acquire(ctxT(t), "test", rw.db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.exec(ctxT(t), "COPY "+itSchema+".things FROM STDIN DELIMITER '|'")
	s.release()
	if err != nil {
		t.Logf("COPY FROM STDIN past the guard: %v", err)
	}
	if after := countRows(t, db, itSchema+".things"); after != before {
		t.Errorf("things: %d rows before, %d after", before, after)
	}
}

func TestNamespaces(t *testing.T) {
	c := openTest(t, false)
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	defaults := 0
	for _, n := range nss {
		names = append(names, n.Name)
		if n.Default {
			defaults++
			if n.Name != "public" {
				t.Errorf("default namespace = %q, want public", n.Name)
			}
		}
	}
	if !strings.Contains(strings.Join(names, ","), itSchema) || defaults != 1 {
		t.Errorf("Namespaces = %v, want %s and one default", names, itSchema)
	}
	colls, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: itSchema})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, col := range colls {
		if col.Ref.Name == "things" {
			found = col.Comment == "things to test" && col.Type == driver.CollectionTable
		}
	}
	if !found {
		t.Errorf("Collections(%s) = %+v, want things with its comment", itSchema, colls)
	}
	if _, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "no_such_schema"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections(missing) = %v, want ErrNotFound", err)
	}
	// A namespace picks the search path; read-only too.
	for _, ro := range []bool{false, true} {
		rs, err := c.Query(ctxT(t), driver.Query{Namespace: itSchema, Text: "SELECT label FROM things WHERE id = 1", ReadOnly: ro})
		if err != nil {
			t.Fatal(err)
		}
		rows, _, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil || len(rows) != 1 || rows[0][0].String() != "in-it-schema" {
			t.Errorf("Query(namespace, ro=%v) = %v, %v", ro, rows, err)
		}
	}
	if _, err := c.Query(ctxT(t), driver.Query{Namespace: "bad\x00name", Text: "SELECT 1"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Query(namespace with NUL) = %v, want ErrSyntax", err)
	}
}

func TestTypesRoundTrip(t *testing.T) {
	db := adminDB()
	defer func() { _ = db.Close() }()
	mustExec(t, db, "CREATE TABLE "+itSchema+".types (id INT PRIMARY KEY, f FLOAT, n NUMERIC(10,2), w NUMERIC(37,15), b BOOLEAN, "+
		"v VARCHAR(20), lv LONG VARCHAR(2000000), d DATE, ts TIMESTAMP, tz TIMESTAMPTZ, tm TIME, bin VARBINARY(4), u UUID, arr ARRAY[INT])")
	mustExec(t, db, "INSERT INTO "+itSchema+".types VALUES (1, 1.25, 12.50, 123456789012345678.123456789012345, true, 'héllo', "+
		"REPEAT('x'::LONG VARCHAR(2000000), 1100000), '2026-01-02', '2026-01-02 03:04:05.123456', '2026-01-02 03:04:05+02', '13:14:15.5', HEX_TO_BINARY('00ff'), "+
		"'6b1c9f5e-0000-4000-8000-000000000000', ARRAY[1,2,3])")
	c := openTest(t, false)
	rs, err := c.Query(ctxT(t), driver.Query{Text: "SELECT * FROM " + itSchema + ".types"})
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, err %v", rows, err)
	}
	if w := strings.Join(st.Warnings, "|"); len(st.Warnings) != 2 || !strings.Contains(w, "cut") || !strings.Contains(w, `"w"`) {
		t.Errorf("Stats.Warnings = %v, want the cut warning and the NUMERIC precision warning for column w", st.Warnings)
	}
	r := rows[0]
	want := map[int]string{1: "1.25", 2: "12.50", 4: "true", 5: "héllo", 10: "13:14:15.5", 12: "6b1c9f5e-0000-4000-8000-000000000000", 13: "[1,2,3]"}
	for i, w := range want {
		if r[i].String() != w {
			t.Errorf("column %d = %q (%s), want %q", i, r[i].String(), r[i].Type(), w)
		}
	}
	if s, _ := r[6].AsText(); len(s) != maxValueBytes {
		t.Errorf("LONG VARCHAR cut to %d bytes, want %d", len(s), maxValueBytes)
	}
	for i, w := range map[int]time.Time{
		7: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		8: time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC),
		9: time.Date(2026, 1, 2, 1, 4, 5, 0, time.UTC),
	} {
		if got, ok := r[i].AsTime(); !ok || !got.Equal(w) {
			t.Errorf("column %d = %v, want %v", i, r[i], w)
		}
	}
	if b, _ := r[11].AsBytes(); string(b) != "\x00\xff" {
		t.Errorf("VARBINARY = %q", b)
	}
	if r[13].Type() != driver.TypeString {
		t.Errorf("ARRAY column type = %s, want string (its text form)", r[13].Type())
	}
	// Browse reads the wide NUMERIC as text: exact.
	brs, err := c.Browse(ctxT(t), driver.CollectionRef{Namespace: itSchema, Name: "types"}, driver.BrowseOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	brows, _, err := driver.Collect(ctxT(t), brs, 0)
	if err != nil || len(brows) != 1 {
		t.Fatal(rows, err)
	}
	if got := brows[0][3]; got.Type() != driver.TypeDecimal || got.String() != "123456789012345678.123456789012345" {
		t.Errorf("Browse wide NUMERIC = %s %q, want the exact decimal", got.Type(), got.String())
	}
	schema, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: itSchema, Name: "types"})
	if err != nil || len(schema.Fields) != 14 || schema.Fields[2].Type != driver.TypeDecimal || schema.PrimaryKey[0] != "id" {
		t.Errorf("Describe(types) = %+v, %v", schema, err)
	}
}

func TestMutatorExtras(t *testing.T) {
	db := adminDB()
	defer func() { _ = db.Close() }()
	mustExec(t, db, "CREATE TABLE "+itSchema+".auto (id AUTO_INCREMENT PRIMARY KEY, label VARCHAR(20))")
	c := openTest(t, false)
	ref := driver.CollectionRef{Namespace: itSchema, Name: "auto"}
	key, err := c.Insert(ctxT(t), ref, driver.Object{{Key: "label", Value: driver.String("first")}})
	if err != nil || len(key) != 1 || key[0].Key != "id" {
		t.Fatalf("Insert(auto key) = %v, %v; want the generated id", key, err)
	}
	if n := countRows(t, db, itSchema+".auto WHERE label = 'first'"); n != 1 {
		t.Errorf("Insert(auto key) left %d rows labelled first, want 1", n)
	}
	mustExec(t, db, "CREATE TABLE "+itSchema+".dups (id INT PRIMARY KEY, label VARCHAR(20))")
	dups := driver.CollectionRef{Namespace: itSchema, Name: "dups"}
	if _, err := c.Insert(ctxT(t), dups, driver.Object{{Key: "id", Value: driver.Int(100)}, {Key: "label", Value: driver.String("a")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Insert(ctxT(t), dups, driver.Object{{Key: "id", Value: driver.Int(100)}, {Key: "label", Value: driver.String("dup")}}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("Insert(duplicate key) = %v, want ErrConstraint", err)
	}
	mustExec(t, db, "INSERT INTO "+itSchema+".dups (id, label) VALUES (100, 'b')") // the key is not enforced
	if err := c.Update(ctxT(t), dups, driver.Object{{Key: "id", Value: driver.Int(100)}}, driver.Object{{Key: "label", Value: driver.String("x")}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Update(duplicated key) = %v, want ErrConflict", err)
	}
	if n := countRows(t, db, itSchema+".dups WHERE label = 'x'"); n != 0 {
		t.Errorf("a conflicting Update changed %d rows, want none", n)
	}
	mustExec(t, db, "CREATE TABLE "+itSchema+".comp (a INT, b INT, label VARCHAR(20), PRIMARY KEY (a, b))")
	comp := driver.CollectionRef{Namespace: itSchema, Name: "comp"}
	if _, err := c.Insert(ctxT(t), comp, driver.Object{{Key: "a", Value: driver.Int(1)}, {Key: "label", Value: driver.String("x")}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Insert(missing key part) = %v, want ErrSyntax", err)
	}
	if n := countRows(t, db, itSchema+".comp"); n != 0 {
		t.Errorf("Insert(missing key part) wrote %d rows", n)
	}
	if _, err := c.Insert(ctxT(t), driver.CollectionRef{Namespace: itSchema, Name: "nope"}, driver.Object{{Key: "a", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Insert(missing table) = %v, want ErrNotFound", err)
	}
}

func TestTxRules(t *testing.T) {
	c := openTest(t, false)
	tx, err := c.Begin(ctxT(t), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, text := range []string{"CREATE TABLE x (a INT)", "COMMIT", "TRUNCATE TABLE tobira_people", "SET SESSION AUTOCOMMIT TO ON"} {
		if _, err := tx.Query(ctxT(t), driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Tx.Query(%q) = %v, want ErrUnsupported", text, err)
		}
	}
	if _, err := c.Begin(ctxT(t), driver.TxOptions{Isolation: "snapshot"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Begin(snapshot) = %v, want ErrUnsupported", err)
	}
	ser, err := c.Begin(ctxT(t), driver.TxOptions{Isolation: "serializable", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ser.Query(ctxT(t), driver.Query{Text: "DELETE FROM tobira_people"}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only Tx write = %v, want ErrReadOnly", err)
	}
	_ = ser.Rollback(ctxT(t))
	for _, text := range []string{"COMMIT", "START TRANSACTION", "SET SESSION AUTOCOMMIT TO OFF"} {
		if _, err := c.Query(ctxT(t), driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Query(%q) = %v, want ErrUnsupported", text, err)
		}
	}
}

func TestExplainRules(t *testing.T) {
	c := openTest(t, false)
	plan, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM tobira_people WHERE id = 1"}, driver.ExplainOptions{})
	if err != nil || !strings.Contains(plan.Text, "tobira_people") {
		t.Errorf("Explain = %+v, %v", plan, err)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "DELETE FROM tobira_people"}, driver.ExplainOptions{}); err != nil {
		t.Errorf("Explain(delete) on a writable conn = %v, want the plan", err)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "DELETE FROM tobira_people", ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Explain(delete, read-only) = %v, want ErrReadOnly", err)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "ANALYZE DELETE FROM tobira_people"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Explain(ANALYZE …) = %v, want ErrSyntax", err)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Explain(Analyze) = %v, want ErrUnsupported", err)
	}
}
