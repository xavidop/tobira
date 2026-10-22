//go:build integration

package mysql

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"database/sql"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestOpenURLForm(t *testing.T) {
	c := openDSN(t, testURL, false)
	rows, _, _ := collect(t, c, driver.Query{Text: "SELECT DATABASE(), @@SESSION.time_zone"})
	if rows[0][0].String() != "tobira" || rows[0][1].String() != "+00:00" {
		t.Errorf("DATABASE(), time_zone = %s, %s", rows[0][0], rows[0][1])
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := ctxT(t)
	_, err := New().Open(ctx, driver.ConnConfig{DSN: "root:tobira@tcp(" + testHostPort + ")/tobira_no_such_db"})
	var de *driver.Error
	if !errors.Is(err, driver.ErrNotFound) || !errors.As(err, &de) || de.Code != "1049" {
		t.Errorf("unknown database: want ErrNotFound 1049, got %v", err)
	}
	_, err = New().Open(ctx, driver.ConnConfig{DSN: "mysql://root:wrong-secret-pw@" + testHostPort + "/tobira"})
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("bad password: want ErrAuth, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "wrong-secret-pw") {
		t.Errorf("bad password error leaks it: %v", err)
	}
}

func TestNamespaces(t *testing.T) {
	c := openMy(t, false)
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]driver.Namespace{}
	for _, n := range nss {
		by[n.Name] = n
		if n.Label != "database" {
			t.Errorf("%s: label %q", n.Name, n.Label)
		}
	}
	if !by["tobira"].Default || by["tobira_ns"].Default {
		t.Errorf("defaults: tobira %+v, tobira_ns %+v", by["tobira"], by["tobira_ns"])
	}
	for _, sys := range []string{"information_schema", "mysql", "performance_schema", "sys"} {
		if _, ok := by[sys]; ok {
			t.Errorf("system database %q listed", sys)
		}
	}
}

func TestCollections(t *testing.T) {
	c := openMy(t, true)
	for _, ns := range []string{"", "tobira"} {
		cols, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: ns})
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]driver.Collection{}
		for _, col := range cols {
			by[col.Ref.Name] = col
			if col.Ref.Namespace != "tobira" {
				t.Errorf("%s: namespace %q", col.Ref.Name, col.Ref.Namespace)
			}
		}
		if p := by["tobira_people"]; p.Type != driver.CollectionTable || p.RowEstimate < 0 {
			t.Errorf("tobira_people = %+v", p)
		}
		if v := by["tobira_people_view"]; v.Type != driver.CollectionView || v.RowEstimate != -1 || v.Comment != "" {
			t.Errorf("tobira_people_view = %+v", v)
		}
		if o := by["tobira_orders"]; o.Comment != "orders table" {
			t.Errorf("tobira_orders = %+v", o)
		}
	}
	cols, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "tobira_ns"})
	if err != nil || len(cols) != 1 || cols[0].Ref != (driver.CollectionRef{Namespace: "tobira_ns", Name: "things"}) {
		t.Errorf("tobira_ns collections = %+v, %v", cols, err)
	}
}

func TestDescribeOrders(t *testing.T) {
	c := openMy(t, false)
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_orders"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref.Namespace != "tobira" || s.Type != driver.CollectionTable || s.Comment != "orders table" || s.SizeBytes < 0 {
		t.Errorf("schema header = %+v", s)
	}
	if !slices.Equal(s.PrimaryKey, []string{"id"}) {
		t.Errorf("primary key = %v", s.PrimaryKey)
	}
	by := map[string]driver.Field{}
	for _, f := range s.Fields {
		by[f.Name] = f
	}
	if f := by["total"]; f.Type != driver.TypeDecimal || f.NativeType != "decimal(8,2)" || f.Default != "0.00" || !f.Nullable {
		t.Errorf("total = %+v", f)
	}
	if f := by["doubled"]; !f.Generated || !strings.Contains(f.Default, "total") {
		t.Errorf("doubled = %+v", f)
	}
	if f := by["note"]; f.Default != "'n/a'" || f.Comment != "free text" || f.Generated {
		t.Errorf("note = %+v", f)
	}
	if f := by["created"]; f.Type != driver.TypeTime || f.Default != "CURRENT_TIMESTAMP" || f.Generated {
		t.Errorf("created = %+v", f)
	}
	if f := by["person_id"]; f.Nullable || f.Type != driver.TypeInt {
		t.Errorf("person_id = %+v", f)
	}
	ix := map[string]driver.Index{}
	for _, i := range s.Indexes {
		ix[i.Name] = i
	}
	if i := ix["PRIMARY"]; !i.Primary || !i.Unique || i.Method != "btree" {
		t.Errorf("PRIMARY = %+v", i)
	}
	if i := ix["tobira_orders_person_total"]; i.Unique || !slices.Equal(i.Fields, []string{"person_id", "total"}) {
		t.Errorf("person_total = %+v", i)
	}
	if i := ix["tobira_orders_note"]; !i.Unique {
		t.Errorf("note index = %+v", i)
	}
	if len(s.ForeignKeys) != 1 {
		t.Fatalf("foreign keys = %+v", s.ForeignKeys)
	}
	fk := s.ForeignKeys[0]
	if fk.Name != "fk_orders_person" || !slices.Equal(fk.Fields, []string{"person_id"}) ||
		fk.References != (driver.CollectionRef{Namespace: "tobira", Name: "tobira_people"}) || !slices.Equal(fk.RefFields, []string{"id"}) {
		t.Errorf("fk = %+v", fk)
	}
}

func TestDescribePeopleTypes(t *testing.T) {
	c := openMy(t, true)
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: "tobira", Name: "tobira_people"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range s.Fields {
		if f.Name == "active" && (f.Type != driver.TypeBool || f.NativeType != "tinyint(1)") {
			t.Errorf("active = %+v, want Bool tinyint(1)", f)
		}
	}
	v, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_people_view"})
	if err != nil || v.Type != driver.CollectionView || v.RowEstimate != -1 || v.SizeBytes != -1 {
		t.Errorf("view = %+v, %v", v, err)
	}
	_, err = c.Describe(ctxT(t), driver.CollectionRef{Namespace: "tobira_ns", Name: "tobira_people"})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("wrong namespace: want ErrNotFound, got %v", err)
	}
}

// TestValueTypes reads every mapped type through the text protocol (no
// parameters) and the binary protocol (a parameter), on writable and
// read-only paths.
func TestValueTypes(t *testing.T) {
	db := admin(t)
	mustExec(t, db, `CREATE TABLE tobira_types (
		id INT PRIMARY KEY, ti TINYINT, b TINYINT(1), si SMALLINT, bi BIGINT, ubi BIGINT UNSIGNED,
		de DECIMAL(10,3), fl FLOAT, db DOUBLE, dt DATETIME, ts TIMESTAMP NULL, d DATE, tm TIME, y YEAR,
		bt BIT(16), js JSON, bl BLOB, vb VARBINARY(4), vc VARCHAR(10), tx TEXT, en ENUM('a','b'), st SET('x','y'))`)
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_types") })
	mustExec(t, db, `INSERT INTO tobira_types VALUES (1, -5, 1, 300, -9000000000, 18446744073709551615,
		12.500, 0.1, 2.25, '2026-01-02 03:04:05', '2026-01-02 03:04:05', '2026-01-02', '838:59:59', 2026,
		b'0000000100000010', '{"a": [1, 2]}', x'0001', x'ff', 'hé', 'text', 'b', 'x,y')`)
	mustExec(t, db, "INSERT INTO tobira_types (id) VALUES (2)")
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := map[string]driver.Value{
		"id": driver.Int(1), "ti": driver.Int(-5), "b": driver.Int(1), "si": driver.Int(300),
		"bi": driver.Int(-9000000000), "ubi": driver.Decimal("18446744073709551615"),
		"de": driver.Decimal("12.500"), "fl": driver.Float(0.1), "db": driver.Float(2.25),
		"dt": driver.Time(ts), "ts": driver.Time(ts), "d": driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
		"tm": driver.String("838:59:59"), "y": driver.Int(2026), "bt": driver.Int(258),
		"js": driver.JSON([]byte(`{"a": [1, 2]}`)), "bl": driver.Bytes([]byte{0, 1}), "vb": driver.Bytes([]byte{0xff}),
		"vc": driver.String("hé"), "tx": driver.String("text"), "en": driver.String("b"), "st": driver.String("x,y"),
	}
	wantType := map[string]driver.Type{
		"ubi": driver.TypeInt, "de": driver.TypeDecimal, "fl": driver.TypeFloat, "dt": driver.TypeTime, "ts": driver.TypeTime,
		"js": driver.TypeJSON, "bl": driver.TypeBytes, "vb": driver.TypeBytes, "vc": driver.TypeString, "tm": driver.TypeString,
	}
	for _, tc := range []struct {
		name string
		c    *conn
		q    driver.Query
	}{
		{"text protocol", openMy(t, false), driver.Query{Text: "SELECT * FROM tobira_types ORDER BY id"}},
		{"binary protocol", openMy(t, false), driver.Query{Text: "SELECT * FROM tobira_types WHERE id >= ? ORDER BY id", Params: []driver.Value{driver.Int(1)}}},
		{"read-only", openMy(t, true), driver.Query{Text: "SELECT * FROM tobira_types ORDER BY id"}},
	} {
		rows, _, cols := collect(t, tc.c, tc.q)
		if len(rows) != 2 {
			t.Fatalf("%s: %d rows", tc.name, len(rows))
		}
		for i, col := range cols {
			got := rows[0][i]
			if w := want[col.Name]; got.Type() != w.Type() || !drivertest.EqualValue(w, got) {
				t.Errorf("%s: %s = %s (%s), want %s (%s)", tc.name, col.Name, got, got.Type(), w, w.Type())
			}
			if tm, ok := got.AsTime(); ok && tm.Location() != time.UTC {
				t.Errorf("%s: %s not UTC: %v", tc.name, col.Name, tm)
			}
			if wt, ok := wantType[col.Name]; ok && col.Type != wt {
				t.Errorf("%s: column %s type %s, want %s", tc.name, col.Name, col.Type, wt)
			}
			if i > 0 && !rows[1][i].IsNull() {
				t.Errorf("%s: %s in the NULL row = %s", tc.name, col.Name, rows[1][i])
			}
		}
	}
}

func TestParamTypes(t *testing.T) {
	c := openMy(t, false)
	ts := time.Date(2026, 5, 6, 7, 8, 9, 0, time.FixedZone("x", 7200))
	rows, _, _ := collect(t, c, driver.Query{
		Text:   "SELECT ?, ?, CAST(? AS DECIMAL(5,2)), ?, ?, CAST(? AS DATETIME), ?, CAST(? AS JSON), ? IS NULL",
		Params: []driver.Value{driver.Int(7), driver.Float(1.5), driver.Decimal("1.25"), driver.String("s"), driver.Bytes([]byte("b")), driver.Time(ts), driver.Bool(true), driver.JSON([]byte(`{"a":1}`)), driver.Null()},
	})
	r := rows[0]
	if !drivertest.EqualValue(driver.Int(7), r[0]) || !drivertest.EqualValue(driver.Float(1.5), r[1]) ||
		r[2].Type() != driver.TypeDecimal || r[2].String() != "1.25" || r[3].String() != "s" {
		t.Errorf("scalars = %v", r[:4])
	}
	if tm, ok := r[5].AsTime(); !ok || !tm.Equal(ts) {
		t.Errorf("time = %s, want %v", r[5], ts.UTC())
	}
	if !drivertest.EqualValue(driver.Int(1), r[6]) || !drivertest.EqualValue(driver.JSON([]byte(`{"a":1}`)), r[7]) || !drivertest.EqualValue(driver.Int(1), r[8]) {
		t.Errorf("bool, json, null = %v", r[6:])
	}
	err := queryErr(t, c, driver.Query{Text: "SELECT ?", Params: []driver.Value{driver.Vector([]float32{1})}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("vector param: want ErrUnsupported, got %v", err)
	}
}

func TestRowsAffectedAndWrites(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	c := openMy(t, false)
	_, st, _ := collect(t, c, driver.Query{Text: "UPDATE " + tbl + " SET name = 'x' WHERE id <= 3;"})
	if st.RowsAffected != 3 {
		t.Errorf("RowsAffected = %d, want 3", st.RowsAffected)
	}
	_, st, _ = collect(t, c, driver.Query{Text: "DELETE FROM " + tbl + " WHERE id = ?", Params: []driver.Value{driver.Int(1)}})
	if st.RowsAffected != 1 {
		t.Errorf("RowsAffected = %d, want 1", st.RowsAffected)
	}
}

func TestMultipleStatementsRejected(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	before := tableState(t, db, tbl)
	c := openMy(t, false)
	err := queryErr(t, c, driver.Query{Text: "DO 1; DELETE FROM " + tbl})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("want ErrSyntax from the server, got %v", err)
	}
	if got := tableState(t, db, tbl); got != before {
		t.Errorf("second statement ran: %s, was %s", got, before)
	}
}

func TestQueryErrorMapping(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	c := openMy(t, false)
	for _, tc := range []struct {
		text string
		kind error
		code string
	}{
		{"SELECT * FROM nope", driver.ErrNotFound, "1146"},
		{"SELEC 1", driver.ErrSyntax, "1064"},
		{"INSERT INTO " + tbl + " SELECT * FROM " + tbl + " WHERE id = 1", driver.ErrConstraint, "1062"},
		{"INSERT INTO tobira_orders (person_id) VALUES (999999)", driver.ErrConstraint, "1452"},
	} {
		err := queryErr(t, c, driver.Query{Text: tc.text})
		var de *driver.Error
		if !errors.Is(err, tc.kind) || !errors.As(err, &de) || de.Code != tc.code || de.Driver != Name {
			t.Errorf("%q: want %v %s, got %v", tc.text, tc.kind, tc.code, err)
		}
	}
}

func TestNamespaceQuery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dsn    string
		ro     bool
		wantDB string
	}{
		{"writable", testDSN, false, "tobira"},
		{"writable, no default database", "root:tobira@tcp(" + testHostPort + ")/", false, ""},
		{"read-only", testDSN, true, "tobira"},
	} {
		c := openDSN(t, tc.dsn, tc.ro)
		c.db.SetMaxOpenConns(1)
		rows, _, _ := collect(t, c, driver.Query{Text: "SELECT label FROM things", Namespace: "tobira_ns"})
		if len(rows) != 1 || rows[0][0].String() != "in-ns" {
			t.Errorf("%s: namespaced rows = %v", tc.name, rows)
		}
		rows, _, _ = collect(t, c, driver.Query{Text: "SELECT DATABASE()"})
		got := rows[0][0].String()
		if rows[0][0].IsNull() {
			got = ""
		}
		if got != tc.wantDB {
			t.Errorf("%s: DATABASE() after a namespaced query = %q, want %q", tc.name, got, tc.wantDB)
		}
	}
}

// bigQuery yields a billion rows, streamed: reading it to the end would take
// far longer than the tests allow.
const bigQuery = "SELECT a.n FROM tobira_big a, tobira_big b, tobira_big c"

func TestLimitStopsEarly(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openMy(t, ro)
		start := time.Now()
		rows, st, _ := collect(t, c, driver.Query{Text: bigQuery, Limit: 5})
		if len(rows) != 5 || !st.Truncated {
			t.Errorf("read-only %v: %d rows, truncated %v", ro, len(rows), st.Truncated)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("read-only %v: Limit 5 over a huge result took %v", ro, d)
		}
		rows, st, _ = collect(t, c, driver.Query{Text: "SELECT n FROM tobira_big ORDER BY n", Limit: 1000})
		if len(rows) != 1000 || st.Truncated {
			t.Errorf("read-only %v: exact limit: %d rows, truncated %v", ro, len(rows), st.Truncated)
		}
	}
}

func TestCloseMidStreamDoesNotDrain(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openMy(t, ro)
		rs, err := c.Query(ctxT(t), driver.Query{Text: bigQuery})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := rs.Next(ctxT(t)); !ok || err != nil {
			t.Fatalf("first row: %v %v", ok, err)
		}
		start := time.Now()
		_ = rs.Close()
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("read-only %v: Close took %v", ro, d)
		}
		if err := c.Ping(ctxT(t)); err != nil {
			t.Errorf("read-only %v: Ping after Close: %v", ro, err)
		}
	}
}

func TestCancelMidQuery(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openMy(t, ro)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		start := time.Now()
		_, err := c.Query(ctx, driver.Query{Text: "SELECT SLEEP(10)"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("read-only %v: want context.Canceled, got %v", ro, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("read-only %v: cancel took %v", ro, d)
		}
		if err := c.Ping(ctxT(t)); err != nil {
			t.Errorf("read-only %v: Ping after cancel: %v", ro, err)
		}
	}
}

// TestWritableSessionStateDoesNotLeak: a writable statement that changes the
// session (USE, SET, START TRANSACTION, LOCK TABLES) must not leave that
// state on a pooled connection for the next call.
func TestWritableSessionStateDoesNotLeak(t *testing.T) {
	db := admin(t)
	tbl := scratch(t, db)
	c := openMy(t, false)
	c.db.SetMaxOpenConns(1) // a reused session would be the same one
	collect(t, c, driver.Query{Text: "USE tobira_ns"})
	if rows, _, _ := collect(t, c, driver.Query{Text: "SELECT DATABASE()"}); rows[0][0].String() != "tobira" {
		t.Errorf("USE leaked: DATABASE() = %s", rows[0][0])
	}
	collect(t, c, driver.Query{Text: "SET SESSION sql_mode = 'NO_BACKSLASH_ESCAPES'"})
	if rows, _, _ := collect(t, c, driver.Query{Text: "SELECT @@SESSION.sql_mode"}); strings.Contains(rows[0][0].String(), "NO_BACKSLASH_ESCAPES") {
		t.Errorf("SET SESSION leaked: sql_mode = %s", rows[0][0])
	}
	collect(t, c, driver.Query{Text: "SET SESSION sql_mode = 'ANSI_QUOTES'", Namespace: "tobira_ns"})
	if rows, _, _ := collect(t, c, driver.Query{Text: "SELECT @@SESSION.sql_mode"}); strings.Contains(rows[0][0].String(), "ANSI_QUOTES") {
		t.Errorf("namespaced SET SESSION leaked: sql_mode = %s", rows[0][0])
	}
	collect(t, c, driver.Query{Text: "START TRANSACTION"})
	collect(t, c, driver.Query{Text: "DELETE FROM " + tbl + " WHERE id = 1"})
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM " + tbl + " WHERE id = 1").Scan(&n); err != nil || n != 0 {
		t.Errorf("a DELETE after START TRANSACTION is not committed (rows %d, %v): it ran inside a leftover transaction", n, err)
	}
	collect(t, c, driver.Query{Text: "LOCK TABLES " + tbl + " READ"})
	if err := queryErr(t, c, driver.Query{Text: "SELECT COUNT(*) FROM tobira_people"}); err != nil {
		t.Errorf("LOCK TABLES leaked into the next call: %v", err)
	}
}

func TestLimitStopsEarlyWith(t *testing.T) {
	c := openMy(t, false)
	start := time.Now()
	rows, st, _ := collect(t, c, driver.Query{
		Text:  "WITH x AS (SELECT n FROM tobira_big) SELECT a.n FROM x a, x b, x c",
		Limit: 5,
	})
	if len(rows) != 5 || !st.Truncated {
		t.Errorf("%d rows, truncated %v", len(rows), st.Truncated)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Limit 5 over a huge WITH result took %v", d)
	}
}

// TestWritableCallsDoNotPoolSessions: every user statement's connection is
// discarded, so a GET_LOCK taken by a SELECT on a writable connection is
// released when the call ends.
func TestWritableCallsDoNotPoolSessions(t *testing.T) {
	db := admin(t)
	c := openMy(t, false)
	if rows, _, _ := collect(t, c, driver.Query{Text: "SELECT GET_LOCK('tobira_rw_lock', 0)"}); rows[0][0].String() != "1" {
		t.Fatalf("GET_LOCK = %s", rows[0][0])
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var free sql.NullInt64
		if err := db.QueryRow("SELECT IS_FREE_LOCK('tobira_rw_lock')").Scan(&free); err != nil {
			t.Fatal(err)
		}
		if free.Valid && free.Int64 == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the lock taken by a writable SELECT is still held after the call")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
