//go:build integration

package libsql

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestQueryPeopleTypes(t *testing.T) {
	c := openLibsql(t, true)
	rows, st, cols := run(t, c, driver.Query{Text: "select id, name, score, active, created_at, tags, age, 1 + 1 as two, 2.5 as half from tobira_people order by id"})
	if len(rows) != len(drivertest.People.Rows) || st.RowsAffected != -1 {
		t.Fatalf("rows=%d stats=%+v", len(rows), st)
	}
	want := []driver.Type{driver.TypeInt, driver.TypeString, driver.TypeDecimal, driver.TypeBool, driver.TypeTime, driver.TypeJSON, driver.TypeInt, driver.TypeInt, driver.TypeFloat}
	for i, w := range want {
		if cols[i].Type != w {
			t.Errorf("col %s type = %v want %v", cols[i].Name, cols[i].Type, w)
		}
	}
	if cols[2].NativeType != "DECIMAL(10,2)" {
		t.Errorf("native = %q", cols[2].NativeType)
	}
	for _, r := range rows {
		for i, w := range want {
			if !r[i].IsNull() && r[i].Type() != w {
				t.Fatalf("row %v col %d: %v want %v", r[0], i, r[i].Type(), w)
			}
		}
	}
	blob, _, _ := run(t, c, driver.Query{Text: "select x'00ff10' as b, null as n"})
	if b, ok := blob[0][0].AsBytes(); !ok || string(b) != "\x00\xff\x10" {
		t.Errorf("blob = %v", blob[0][0])
	}
	if !blob[0][1].IsNull() {
		t.Errorf("null = %v", blob[0][1])
	}
}

func TestQueryLimitAndRowsAffected(t *testing.T) {
	c := openLibsql(t, false)
	rows, st, _ := run(t, c, driver.Query{Text: "select id from tobira_people order by id", Limit: 5})
	if len(rows) != 5 || !st.Truncated {
		t.Fatalf("rows=%d truncated=%v", len(rows), st.Truncated)
	}
	table := scratch(t, c)
	_, st, cols := run(t, c, driver.Query{Text: "update " + table + " set age = 1 where id <= 3"})
	if len(cols) != 0 || st.RowsAffected != 3 {
		t.Fatalf("cols=%d stats=%+v", len(cols), st)
	}
	// DDL after a write reports no stale change count.
	_, st, _ = run(t, c, driver.Query{Text: "create index " + table + "_age on " + table + " (age)"})
	if st.RowsAffected != 0 {
		t.Errorf("create index rows affected = %d", st.RowsAffected)
	}
	rows, _, cols = run(t, c, driver.Query{Text: "insert into " + table + " (id, name, email) values (777, 'n', 'n777@x') returning id"})
	if len(rows) != 1 || cols[0].Name != "id" {
		t.Errorf("returning rows = %v", rows)
	}
}

func TestDescribeCatalog(t *testing.T) {
	c := openLibsql(t, true)
	ctx := context.Background()
	s, err := c.Describe(ctx, driver.CollectionRef{Name: "tobira_orders"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref.Namespace != "main" || s.Type != driver.CollectionTable || !strings.Contains(s.DDL, "CREATE TABLE tobira_orders") {
		t.Errorf("schema = %+v", s)
	}
	byName := map[string]driver.Field{}
	for _, f := range s.Fields {
		byName[f.Name] = f
	}
	if !byName["id"].AutoIncrement || byName["person_id"].Nullable || byName["total"].Type != driver.TypeDecimal ||
		byName["total"].Default != "0" || !byName["doubled"].Generated || byName["note"].Default != "'n/a'" {
		t.Errorf("fields = %+v", s.Fields)
	}
	if !slices.Equal(s.PrimaryKey, []string{"id"}) {
		t.Errorf("pk = %v", s.PrimaryKey)
	}
	var idx []string
	for _, i := range s.Indexes {
		idx = append(idx, i.Name+":"+strings.Join(i.Fields, ",")+":"+map[bool]string{true: "u", false: "-"}[i.Unique])
	}
	slices.Sort(idx)
	if !slices.Equal(idx, []string{"tobira_orders_note:note:u", "tobira_orders_person_total:person_id,total:-"}) {
		t.Errorf("indexes = %v", idx)
	}
	if len(s.ForeignKeys) != 1 || s.ForeignKeys[0].References.Name != "tobira_people" ||
		!slices.Equal(s.ForeignKeys[0].Fields, []string{"person_id"}) || !slices.Equal(s.ForeignKeys[0].RefFields, []string{"id"}) {
		t.Errorf("fks = %+v", s.ForeignKeys)
	}
	v, err := c.Describe(ctx, driver.CollectionRef{Namespace: "main", Name: "tobira_people_view"})
	if err != nil || v.Type != driver.CollectionView || len(v.Fields) != 2 {
		t.Errorf("view = %+v, %v", v, err)
	}
	for _, name := range []string{"tobira_desc", "tobira_norowid"} {
		d, err := c.Describe(ctx, driver.CollectionRef{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if d.Fields[0].AutoIncrement {
			t.Errorf("%s: k is not a rowid alias", name)
		}
	}
	if _, err := c.Describe(ctx, driver.CollectionRef{Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	colls, err := c.Collections(ctx, driver.NamespaceRef{Name: "main"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, col := range colls {
		names = append(names, col.Ref.Name)
		if strings.HasPrefix(col.Ref.Name, "sqlite_") {
			t.Errorf("internal table listed: %s", col.Ref.Name)
		}
	}
	if !slices.Contains(names, "tobira_people") || !slices.Contains(names, "tobira_people_view") {
		t.Errorf("collections = %v", names)
	}
}

// Explain only compiles: a DELETE explained on a writable connection leaves
// the data as it was.
func TestExplainNeverWrites(t *testing.T) {
	c := openLibsql(t, false)
	ctx := context.Background()
	table := scratch(t, c)
	before := count(t, c, table)
	for _, text := range []string{"delete from " + table, "update " + table + " set name = 'x'", "insert into " + table + " (id, name, email) values (900, 'a', 'b')"} {
		p, err := c.Explain(ctx, driver.Query{Text: text}, driver.ExplainOptions{})
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if p.Text == "" || p.Root == nil {
			t.Errorf("%q: empty plan", text)
		}
	}
	if n := count(t, c, table); n != before {
		t.Errorf("count %d -> %d", before, n)
	}
	p, err := c.Explain(ctx, driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(1)}}, driver.ExplainOptions{})
	if err != nil || !strings.Contains(p.Text, "tobira_people") {
		t.Errorf("plan = %+v, %v", p, err)
	}
	if _, err := c.Explain(ctx, driver.Query{Text: "select 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("analyze: %v", err)
	}
}

func TestMutator(t *testing.T) {
	c := openLibsql(t, false)
	ctx := context.Background()
	table := scratch(t, c) // no primary key: CREATE TABLE AS drops it
	ref := driver.CollectionRef{Name: table}
	mustExec(t, c, "insert into "+table+" (id, name, email) values (1, 'dup', 'dup@x')")
	if err := c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("update matching two rows: %v", err)
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(4242)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("delete of a missing key: %v", err)
	}
	if err := c.Update(ctx, ref, driver.Object{{Key: "nope", Value: driver.Int(1)}}, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown key field: %v", err)
	}
	rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from " + table + " where name = 'x'"})
	if n, _ := rows[0][0].AsInt(); n != 0 {
		t.Errorf("a refused update changed %d rows", n)
	}
	// The generated rowid alias comes back from Insert.
	orders := driver.CollectionRef{Name: "tobira_orders"}
	key, err := c.Insert(ctx, orders, driver.Object{{Key: "person_id", Value: driver.Int(2)}, {Key: "note", Value: driver.String("mutator-test")}})
	if err != nil {
		t.Fatal(err)
	}
	id, ok := key[0].Value.AsInt()
	if len(key) != 1 || key[0].Key != "id" || !ok || id <= 0 {
		t.Fatalf("key = %v", key)
	}
	if err := c.Update(ctx, orders, key, driver.Object{{Key: "total", Value: driver.Decimal("12.50")}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, orders, key); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Insert(ctx, driver.CollectionRef{Name: "tobira_people"}, driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "name", Value: driver.String("a")}, {Key: "email", Value: driver.String("dup@a")}}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("duplicate key insert: %v", err)
	}
	if _, err := c.Insert(ctx, driver.CollectionRef{Name: "tobira_people_view"}, driver.Object{{Key: "id", Value: driver.Int(5)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("insert into a view: %v", err)
	}
}

// An idle transaction outlives sqld's stream timeout (about 10 s) because
// the driver keeps its stream alive.
func TestTxSurvivesIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 13 s")
	}
	c := openLibsql(t, false)
	ctx := context.Background()
	table := scratch(t, c)
	dtx, err := c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, dtx, "update "+table+" set name = 'idle' where id = 1")
	time.Sleep(13 * time.Second)
	rows, _, _ := run(t, dtx, driver.Query{Text: "select name from " + table + " where id = 1"})
	if s, _ := rows[0][0].AsText(); s != "idle" {
		t.Fatalf("name in tx after idling = %q", s)
	}
	if err := dtx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _, _ = run(t, c, driver.Query{Text: "select name from " + table + " where id = 1"})
	if s, _ := rows[0][0].AsText(); s != "idle" {
		t.Errorf("committed name = %q", s)
	}
}

func TestTxSeesItsOwnTablesAndRollsBack(t *testing.T) {
	c := openLibsql(t, false)
	ctx := context.Background()
	dtx, err := c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, dtx, "create table tobira_txonly (id integer primary key, v text)")
	m := dtx.(driver.Mutator)
	key, err := m.Insert(ctx, driver.CollectionRef{Name: "tobira_txonly"}, driver.Object{{Key: "v", Value: driver.String("a")}})
	if err != nil || len(key) != 1 {
		t.Fatalf("insert in tx: %v %v", key, err)
	}
	p, err := dtx.(driver.Explainer).Explain(ctx, driver.Query{Text: "select * from tobira_txonly"}, driver.ExplainOptions{})
	if err != nil || p.Text == "" {
		t.Errorf("explain in tx: %v %v", p, err)
	}
	for _, text := range []string{"commit", ";commit", "begin", "rollback", "END TRANSACTION"} {
		if _, err := dtx.Query(ctx, driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%q: %v", text, err)
		}
	}
	mustExec(t, dtx, "savepoint a")
	mustExec(t, dtx, "rollback to a")
	mustExec(t, dtx, "release a")
	if _, err := dtx.Query(ctx, driver.Query{Text: "select 1", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("read-only query in a read-write tx: %v", err)
	}
	if err := dtx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dtx.Rollback(ctx); err != nil {
		t.Errorf("second rollback: %v", err)
	}
	if _, err := dtx.Query(ctx, driver.Query{Text: "select 1"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("query after rollback: %v", err)
	}
	if tableExists(t, c, "tobira_txonly") {
		t.Error("the rolled-back table exists")
	}
	if _, err := c.Begin(ctx, driver.TxOptions{Isolation: "read committed"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("isolation: %v", err)
	}
}

func TestCloseRollsBackOpenTx(t *testing.T) {
	c := openLibsql(t, false)
	ctx := context.Background()
	table := scratch(t, c)
	other, err := New().Open(ctx, cfgFor(false, rwToken))
	if err != nil {
		t.Fatal(err)
	}
	dtx, err := other.(*conn).Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, dtx, "delete from "+table)
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, table); n != int64(len(drivertest.People.Rows)) {
		t.Errorf("count after Close = %d: the open tx was not rolled back", n)
	}
}

func TestServerErrors(t *testing.T) {
	c := openLibsql(t, false)
	ctx := context.Background()
	_, err := c.Query(ctx, driver.Query{Text: "insert into tobira_people (id, name, email) values (1, 'a', 'b')"})
	var de *driver.Error
	if !errors.Is(err, driver.ErrConstraint) || !errors.As(err, &de) || de.Code != "SQLITE_CONSTRAINT" {
		t.Errorf("constraint: %#v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "select * from nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing table: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "selec 1"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("syntax: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "vacuum"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("vacuum: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "attach 'x.db' as x"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("attach on a writable conn: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "select 1; select 2"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("multiple statements: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "select ?, ?", Params: []driver.Value{driver.Int(1)}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("missing parameter: %v", err)
	}
}

// A cancelled slow query returns at once, and the connection stays usable.
func TestCancelDoesNotWedge(t *testing.T) {
	c := openLibsql(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Query(ctx, driver.Query{Text: "with recursive c(x) as (select 1 union all select x+1 from c where x < 50000000) select count(*) from c"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("cancel took %v", d)
	}
	qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer qcancel()
	if _, err := c.Query(qctx, driver.Query{Text: "select 1"}); err != nil {
		t.Errorf("query after cancel: %v", err)
	}
}

// Every documented URL form reaches the server; the token never shows in an
// error.
func TestDSNForms(t *testing.T) {
	u, err := url.Parse(testURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "libsql://" + u.Host + "?tls=0", Password: driver.NewSecret(rwToken)},
		{DSN: "ws://" + u.Host, Password: driver.NewSecret(rwToken)},
		{DSN: "http://:" + rwToken + "@" + u.Host},
	} {
		cfg.Driver = Name
		c, err := New().Open(ctx, cfg)
		if err != nil {
			t.Errorf("%s: %v", driver.RedactDSN(cfg.DSN), err)
			continue
		}
		if err := c.Ping(ctx); err != nil {
			t.Errorf("ping: %v", err)
		}
		_ = c.Close()
	}
	// The scheme alone picks the driver for libsql:// URLs.
	reg := driver.NewRegistry()
	if err := reg.Add(New()); err != nil {
		t.Fatal(err)
	}
	c, err := reg.Open(ctx, driver.ConnConfig{DSN: "libsql://" + u.Host + "?tls=0", Password: driver.NewSecret(rwToken)})
	if err != nil {
		t.Fatalf("open by scheme: %v", err)
	}
	_ = c.Close()
	_, err = New().Open(ctx, driver.ConnConfig{Driver: Name, DSN: "http://:wrong-token@" + u.Host})
	if !errors.Is(err, driver.ErrAuth) || strings.Contains(err.Error(), "wrong-token") {
		t.Errorf("bad token: %v", err)
	}
}
