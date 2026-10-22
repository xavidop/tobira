//go:build integration

package snowflake

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func ictx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func collect(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row) {
	t.Helper()
	rs, err := c.Query(ictx(t), q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	rows, _, err := driver.Collect(ictx(t), rs, 0)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	return rs.Columns(), rows
}

// Snowflake does not enforce primary keys, so the Mutator enforces its
// one-row contract itself.
func TestMutatorKeySemantics(t *testing.T) {
	db := adminDB()
	defer func() { _ = db.Close() }()
	mustExec(t, db, `CREATE OR REPLACE TABLE `+testNS+`."keyless" ("grp" INT, "v" VARCHAR)`)
	mustExec(t, db, `INSERT INTO `+testNS+`."keyless" VALUES (1, 'a'), (1, 'b'), (2, 'c')`)
	c := openTest(t, false)
	m := c.(driver.Mutator)
	ref := driver.CollectionRef{Namespace: testNS, Name: "keyless"}
	grp := func(n int64) driver.Object { return driver.Object{{Key: "grp", Value: driver.Int(n)}} }
	changes := driver.Object{{Key: "v", Value: driver.String("changed")}}

	if err := m.Update(ictx(t), ref, grp(1), changes); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Update matching two rows: %v, want ErrConflict", err)
	}
	if err := m.Delete(ictx(t), ref, grp(1)); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Delete matching two rows: %v, want ErrConflict", err)
	}
	if err := m.Update(ictx(t), ref, grp(9), changes); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Update matching nothing: %v, want ErrNotFound", err)
	}
	if err := m.Update(ictx(t), ref, grp(2), changes); err != nil {
		t.Errorf("Update matching one row: %v", err)
	}
	_, rows := collect(t, c, driver.Query{Text: `select "grp", "v" from "keyless" order by "grp", "v"`})
	var got []string
	for _, r := range rows {
		got = append(got, r[0].String()+"="+r[1].String())
	}
	if want := []string{"1=a", "1=b", "2=changed"}; !slices.Equal(got, want) {
		t.Errorf("after the refused and the accepted change: %v, want %v (the conflicts were rolled back)", got, want)
	}

	// A duplicate primary key is refused before the server sees a second row.
	people := driver.CollectionRef{Namespace: testNS, Name: "tobira_people"}
	dup := driver.Object{
		{Key: "id", Value: driver.Int(1)}, {Key: "name", Value: driver.String("dup")}, {Key: "email", Value: driver.String("d@x")},
		{Key: "score", Value: driver.Decimal("1.00")}, {Key: "active", Value: driver.Bool(true)},
		{Key: "created_at", Value: driver.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}, {Key: "tags", Value: driver.JSON([]byte(`[]`))},
	}
	if _, err := m.Insert(ictx(t), people, dup); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("Insert of an existing key: %v, want ErrConstraint", err)
	}
	// A new row with JSON into an ARRAY column, read back.
	row := slices.Clone(dup)
	row[0].Value, row[6].Value = driver.Int(5000), driver.JSON([]byte(`["x","y"]`))
	key, err := m.Insert(ictx(t), people, row)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(context.Background(), people, key) })
	if len(key) != 1 || key[0].Key != "id" {
		t.Errorf("Insert returned key %v, want the id", key)
	}
	_, rows = collect(t, c, driver.Query{Text: `select "tags", "score" from "tobira_people" where "id" = 5000`})
	if len(rows) != 1 || !drivertest.EqualValue(driver.JSON([]byte(`["x","y"]`)), rows[0][0]) || rows[0][1].String() != "1.00" {
		t.Errorf("inserted row reads back as %v", rows)
	}
}

// Snowflake commits the open transaction before a DDL statement, so the
// driver refuses DDL inside one.
func TestTxRefusesDDL(t *testing.T) {
	c := openTest(t, false)
	tx, err := c.(driver.Transactor).Begin(ictx(t), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Query(ictx(t), driver.Query{Text: `CREATE TABLE "tx_ddl" ("a" INT)`}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("DDL in a Tx: %v, want ErrUnsupported", err)
	}
	if _, err := tx.Query(ictx(t), driver.Query{Text: "COMMIT"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("COMMIT in a Tx: %v, want ErrUnsupported", err)
	}
	if err := tx.Rollback(ictx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Describe(ictx(t), driver.CollectionRef{Namespace: testNS, Name: "tx_ddl"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("the refused DDL ran: Describe = %v", err)
	}
}

func TestNamespacesAndQueryNamespace(t *testing.T) {
	c := openTest(t, false)
	nss, err := c.Namespaces(ictx(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var def string
	for _, n := range nss {
		names = append(names, n.Name)
		if n.Default {
			def = n.Name
		}
	}
	if !slices.Contains(names, testNS) || !slices.Contains(names, testDB+".OTHER") || def != testNS {
		t.Errorf("Namespaces = %v (default %q)", names, def)
	}
	colls, err := c.Collections(ictx(t), driver.NamespaceRef{Name: testDB + ".OTHER"})
	if err != nil || len(colls) != 1 || colls[0].Ref.Name != "things" {
		t.Errorf("Collections(OTHER) = %+v, %v", colls, err)
	}
	if _, err := c.Collections(ictx(t), driver.NamespaceRef{Name: testDB + ".NOPE"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections(missing schema) = %v, want ErrNotFound", err)
	}
	_, rows := collect(t, c, driver.Query{Text: `select "label" from "things"`, Namespace: testDB + ".OTHER"})
	if len(rows) != 1 || rows[0][0].String() != "in-other" {
		t.Errorf("Query in namespace OTHER = %v", rows)
	}
}

// fakesnow's INFORMATION_SCHEMA.TABLES lists no views (it reports every
// table as BASE TABLE), so views are covered by TestCollectionType only.
func TestBrowseFilters(t *testing.T) {
	c := openTest(t, true)
	b := c.(driver.Browser)
	ref := driver.CollectionRef{Namespace: testNS, Name: "tobira_people"}
	rs, err := b.Browse(ictx(t), ref, driver.BrowseOptions{Filter: driver.Filter{
		Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(3), driver.Int(5), driver.Int(8))}},
		Raw:        `"id" > 3 -- a trailing comment`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ictx(t), rs, 0)
	if err != nil || len(rows) != 2 {
		t.Errorf("Browse(IN and raw) = %d rows, %v; want ids 5 and 8", len(rows), err)
	}
}

// The read-only guard against the fake: reads work, the refusals never
// reach it (fakesnow would run them).
func TestReadOnlyAgainstFakesnow(t *testing.T) {
	ro := openTest(t, true)
	for _, q := range []string{
		`select * from "tobira_people" where "id" = 1`,
		"show tables",
		`describe table "tobira_people"`,
		`with c as (select 1 as x) select x from c`,
	} {
		collect(t, ro, driver.Query{Text: q})
	}
	for _, q := range []string{
		`insert into "tobira_people" ("id") values (9999)`,
		`create table "ro_made" ("a" int)`,
		`select $1 from @some_stage`,
		`select system$cancel_all_queries(1)`,
		`select 1; drop table "tobira_people"`,
	} {
		if _, err := ro.Query(ictx(t), driver.Query{Text: q}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %q: %v, want ErrReadOnly", q, err)
		}
	}
	_, rows := collect(t, ro, driver.Query{Text: `select count(*) from "tobira_people"`})
	if len(rows) != 1 || rows[0][0].String() != "57" {
		t.Errorf("People count after the refusals = %v", rows)
	}
	if _, err := ro.Describe(ictx(t), driver.CollectionRef{Name: "ro_made"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a refused CREATE ran: %v", err)
	}
}

// fakesnow stores NUMBER(38,0) as a 64-bit integer, so a value past int64
// (read as Decimal) is covered by TestConvertValue only.
func TestValueTypes(t *testing.T) {
	db := adminDB()
	defer func() { _ = db.Close() }()
	mustExec(t, db, `CREATE OR REPLACE TABLE `+testNS+`."typed" ("v" VARIANT, "o" OBJECT, "b" BINARY, "d" DATE, "f" FLOAT, `+
		`"n" NUMBER(38,0), "dec" NUMBER(12,4), "ntz" TIMESTAMP_NTZ)`)
	mustExec(t, db, `INSERT INTO `+testNS+`."typed" SELECT PARSE_JSON('{"a":[1,2]}'), OBJECT_CONSTRUCT('k', 'v'), TO_BINARY('CAFE', 'HEX'), `+
		`'2026-01-02', 2.5, 42, 1.5, '2026-01-02 03:04:05.123'`)
	c := openTest(t, true)
	cols, rows := collect(t, c, driver.Query{Text: `select * from "typed"`})
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	want := []driver.Value{
		driver.JSON([]byte(`{"a":[1,2]}`)), driver.JSON([]byte(`{"k":"v"}`)), driver.Bytes([]byte{0xca, 0xfe}),
		driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)), driver.Float(2.5), driver.Int(42),
		driver.Decimal("1.5000"),
		driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)),
	}
	for i, w := range want {
		if !drivertest.EqualValue(w, rows[0][i]) || w.Type() != rows[0][i].Type() {
			t.Errorf("column %s (%s): %#v (%v), want %#v (%v)", cols[i].Name, cols[i].NativeType, rows[0][i], rows[0][i].Type(), w, w.Type())
		}
	}
	if s := rows[0][6].String(); s != "1.5000" {
		t.Errorf("NUMBER(12,4) reads as %q, want the exact 1.5000", s)
	}
}
