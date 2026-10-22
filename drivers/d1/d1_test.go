package d1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func query(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rs.Columns(), rows, st
}

func TestSendsOneStatementToRawWithTypedParams(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, false)
	before := len(f.log())
	_, rows, _ := query(t, c, driver.Query{
		Text:   "select id, name from tobira_people where id = ? or name = ? or score > ? or active = ? or age is ?",
		Params: []driver.Value{driver.Int(7), driver.String("x"), driver.Float(1e9), driver.Bool(true), driver.Null()},
	})
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	reqs := f.log()[before:]
	last := reqs[len(reqs)-1]
	if !strings.HasSuffix(last.Path, "/accounts/"+fakeAccount+"/d1/database/"+fakeDatabase+"/raw") {
		t.Errorf("path = %s, want the /raw endpoint", last.Path)
	}
	if last.Auth != "Bearer "+fakeToken {
		t.Errorf("Authorization = %q", last.Auth)
	}
	if len(last.Params) != 5 || last.Params[1] != "x" || last.Params[4] != nil || last.Params[3] == true {
		t.Errorf("params = %#v (bools go as 0/1)", last.Params)
	}
	for _, r := range reqs {
		if strings.Count(r.SQL, ";") > 0 {
			t.Errorf("sent more than one statement: %q", r.SQL)
		}
	}
}

func TestUnbindableParamsRefused(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, false)
	before := len(f.log())
	for _, v := range []driver.Value{driver.Bytes([]byte{1}), driver.List(driver.Int(1)), driver.Vector([]float32{1})} {
		_, err := c.Query(context.Background(), driver.Query{Text: "select ?", Params: []driver.Value{v}})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s param: %v, want ErrUnsupported", v.Type(), err)
		}
	}
	if n := len(f.log()) - before; n != 0 {
		t.Errorf("%d requests sent for refused params", n)
	}
}

// readOnlyDenied are statements the read-only guard must refuse before
// anything reaches D1: D1 has no read-only session or transaction, so the
// lexical guard is the only layer.
var readOnlyDenied = []string{
	"delete from tobira_people",
	"insert into tobira_people (id, name, email) values (5000, 'a', 'b')",
	"update tobira_people set name = 'x'",
	"replace into tobira_people (id, name, email) values (1, 'a', 'b')",
	"with x as (select 1) delete from tobira_people",
	"with x as (select 1) update tobira_people set name = 'x'",
	"with x as (select 1) insert into tobira_people select * from tobira_people",
	"with x as (select 1) replace into tobira_people select * from tobira_people",
	"attach database 'x.db' as x",
	"detach database x",
	"pragma user_version = 7",
	"pragma foreign_keys = off",
	"pragma main.writable_schema = 1",
	"pragma table_info = 1",
	"explain pragma user_version = 7",
	"/* c */ create table t (a)",
	"drop table tobira_people",
	"alter table tobira_people add column z",
	"vacuum",
	"vacuum into 'x.db'",
	"reindex",
	"analyze",
	"select load_extension('x')",
	"select \"load_extension\"('x')",
	"select [load_extension]('x')",
	"begin",
	"commit",
	"savepoint s",
	"\xef\xbb\xbfdelete from tobira_people",
	"",
	"   ",
}

func TestReadOnlyGuardRefusesBeforeSending(t *testing.T) {
	f := newFakeD1(t)
	ro := f.open(t, true)
	rw := f.open(t, false)
	before := len(f.log())
	for _, q := range readOnlyDenied {
		if _, err := ro.Query(context.Background(), driver.Query{Text: q}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only conn, %q: %v, want ErrReadOnly", q, err)
		}
		if _, err := rw.Query(context.Background(), driver.Query{Text: q, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Query.ReadOnly, %q: %v, want ErrReadOnly", q, err)
		}
	}
	for _, q := range []string{"select 1; delete from tobira_people", "select 1;;", "select 1; -- x\n select 2"} {
		if _, err := ro.Query(context.Background(), driver.Query{Text: q}); err == nil {
			t.Errorf("multi-statement %q passed", q)
		}
		if _, err := rw.Query(context.Background(), driver.Query{Text: q}); err == nil {
			t.Errorf("multi-statement %q passed on a read-write conn", q)
		}
	}
	if n := len(f.log()) - before; n != 0 {
		t.Errorf("%d refused statements reached the server: %+v", n, f.log()[before:])
	}
	if got := f.count(t, "select count(*) from tobira_people"); got != 57 {
		t.Errorf("rows = %d, want 57", got)
	}
	// Reads still pass.
	for _, q := range []string{
		"select count(*) from tobira_people",
		"pragma table_info(tobira_people)",
		"explain query plan select * from tobira_people",
		"with x as (select 1 as a) select replace('ab', 'a', 'c') from x",
		"values (1)",
	} {
		if _, _, _ = query(t, ro, driver.Query{Text: q}); t.Failed() {
			t.Logf("while reading %q", q)
		}
	}
}

func TestTransactionControlRefused(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, false)
	before := len(f.log())
	for _, q := range []string{"begin", "BEGIN IMMEDIATE", "commit", "end", "rollback", "/* x */ begin transaction"} {
		if _, err := c.Query(context.Background(), driver.Query{Text: q}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%q: %v, want ErrUnsupported", q, err)
		}
	}
	if n := len(f.log()) - before; n != 0 {
		t.Errorf("%d transaction statements were sent", n)
	}
	if _, ok := driver.As[driver.Transactor](driver.Conn(c)); ok {
		t.Error("the driver claims transactions; D1's REST API has none")
	}
}

func TestQueryTyping(t *testing.T) {
	f := newFakeD1(t)
	f.mustExec(t, "create table nums (r REAL, d DECIMAL(10,3), b BLOB, j JSON)")
	f.mustExec(t, "insert into nums values (3.0, 2, x'00ff', '{\"a\":1}')")
	c := f.open(t, false)

	types := func(q string) []driver.Type {
		cols, _, _ := query(t, c, driver.Query{Text: q})
		out := make([]driver.Type, len(cols))
		for i, col := range cols {
			out[i] = col.Type
		}
		return out
	}
	eq := func(q string, want ...driver.Type) {
		t.Helper()
		got := types(q)
		if len(got) != len(want) {
			t.Errorf("%q: types %v, want %v", q, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: types %v, want %v", q, got, want)
				return
			}
		}
	}
	people := []driver.Type{driver.TypeInt, driver.TypeString, driver.TypeString, driver.TypeInt, driver.TypeDecimal, driver.TypeBool, driver.TypeTime, driver.TypeJSON}
	eq("select * from tobira_people where id = 1", people...)
	eq("SELECT * FROM main.tobira_people ORDER BY id LIMIT 3", people...)
	eq(`select * from "tobira_people" as p where p.id < 3`, people...)
	eq("select active, created_at, Score from tobira_people where id = 2", driver.TypeBool, driver.TypeTime, driver.TypeDecimal)
	eq("select p.active, p.tags from tobira_people p", driver.TypeBool, driver.TypeJSON)
	eq("select distinct active from tobira_people order by 1", driver.TypeBool)
	// Not a plain column list over one table: runtime types only.
	eq("select active as a from tobira_people where id = 2", driver.TypeInt)
	eq("select active + 0 from tobira_people where id = 2", driver.TypeInt)
	eq("select count(*) from tobira_people", driver.TypeInt)
	eq("select a.active from tobira_people a join tobira_people b on a.id = b.id where a.id = 2", driver.TypeInt)
	eq("select active from tobira_people where id = 2 union all select 5", driver.TypeInt)
	eq("with p as (select * from tobira_people) select active from p where id = 2", driver.TypeInt)

	_, rows, _ := query(t, c, driver.Query{Text: "select * from nums"})
	if v := rows[0][0]; v.Type() != driver.TypeFloat {
		t.Errorf("REAL 3.0 (sent by D1 as 3) = %v (%s), want Float", v, v.Type())
	}
	if v := rows[0][1]; v.Type() != driver.TypeDecimal || v.String() != "2.000" {
		t.Errorf("DECIMAL(10,3) 2 = %v (%s)", v, v.Type())
	}
	if b, ok := rows[0][2].AsBytes(); !ok || len(b) != 2 || b[1] != 0xff {
		t.Errorf("BLOB = %v (%s)", rows[0][2], rows[0][2].Type())
	}
	if v := rows[0][3]; v.Type() != driver.TypeJSON {
		t.Errorf("JSON = %v (%s)", v, v.Type())
	}
}

func TestQueryLimitAndRowsAffected(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, false)
	_, rows, st := query(t, c, driver.Query{Text: "select * from tobira_people order by id", Limit: 5})
	if len(rows) != 5 || !st.Truncated {
		t.Errorf("limit 5: %d rows, truncated %v", len(rows), st.Truncated)
	}
	_, _, st = query(t, c, driver.Query{Text: "update tobira_people set name = name where id < 4"})
	if st.RowsAffected != 3 {
		t.Errorf("RowsAffected = %d, want 3", st.RowsAffected)
	}
}

func TestSimpleSource(t *testing.T) {
	for _, tc := range []struct {
		q     string
		table string
		cols  []string // nil means *
		ok    bool
	}{
		{"select * from t", "t", nil, true},
		{"SELECT * FROM main.t WHERE a = (select max(a) from u) ORDER BY a LIMIT 1;", "t", nil, true},
		{`select "a", [b], c from "T" x where 1`, "T", []string{"a", "b", "c"}, true},
		{"select x.a, b from t as x group by a", "t", []string{"a", "b"}, true},
		{"select all a from t", "t", []string{"a"}, true},
		{"select a as b from t", "", nil, false},
		{"select a b from t", "", nil, false},
		{"select a, * from t", "", nil, false},
		{"select 'a' from t", "", nil, false},
		{"select a from t, u", "", nil, false},
		{"select a from t join u", "", nil, false},
		{"select a from t natural join u", "", nil, false},
		{"select a from t where a in (select a from u) union select 1", "", nil, false},
		{"select a from (select 1 as a)", "", nil, false},
		{"select a from temp.t", "", nil, false},
		{"select a from y.t", "", nil, false},
		{"select y.a from t", "", nil, false},
		{"select a from t x where y.a = 1", "t", []string{"a"}, true},
		{"select a from pragma_table_info('t')", "", nil, false},
		{"with x as (select 1) select * from t", "", nil, false},
		{"values (1)", "", nil, false},
		{"select a from t indexed by i", "", nil, false},
		{"select a from t window w as ()", "", nil, false},
		{"select", "", nil, false},
		{"select distinct", "", nil, false},
		{"select all from t", "", nil, false},
	} {
		table, cols, ok := simpleSource(tc.q)
		if ok != tc.ok || table != tc.table || strings.Join(cols, ",") != strings.Join(tc.cols, ",") || (ok && (cols == nil) != (tc.cols == nil)) {
			t.Errorf("simpleSource(%q) = %q %q %v, want %q %q %v", tc.q, table, cols, ok, tc.table, tc.cols, tc.ok)
		}
	}
}

func TestMutatorByKey(t *testing.T) {
	f := newFakeD1(t)
	f.mustExec(t, "create table dup (a integer, b text)")
	f.mustExec(t, "insert into dup values (1, 'x'), (1, 'y'), (2, 'z')")
	f.mustExec(t, "create view v as select * from dup")
	c := f.open(t, false)
	ctx := context.Background()
	dup := driver.CollectionRef{Name: "dup"}

	err := c.Update(ctx, dup, driver.Object{{Key: "a", Value: driver.Int(1)}}, driver.Object{{Key: "b", Value: driver.String("q")}})
	if !errors.Is(err, driver.ErrConflict) {
		t.Errorf("update matching two rows: %v, want ErrConflict", err)
	}
	if n := f.count(t, "select count(*) from dup where b = 'q'"); n != 0 {
		t.Errorf("the conflicting update changed %d rows", n)
	}
	if err := c.Delete(ctx, dup, driver.Object{{Key: "A", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("delete matching two rows: %v, want ErrConflict", err)
	}
	if n := f.count(t, "select count(*) from dup"); n != 3 {
		t.Errorf("the conflicting delete left %d rows", n)
	}
	if err := c.Update(ctx, dup, driver.Object{{Key: "a", Value: driver.Int(2)}}, driver.Object{{Key: "b", Value: driver.String("w")}}); err != nil {
		t.Errorf("update one row: %v", err)
	}
	if err := c.Delete(ctx, dup, driver.Object{{Key: "a", Value: driver.Int(9)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("delete missing: %v, want ErrNotFound", err)
	}
	if err := c.Update(ctx, dup, driver.Object{{Key: "nope", Value: driver.Int(1)}}, driver.Object{{Key: "b", Value: driver.Null()}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown key field: %v, want ErrNotFound", err)
	}
	if err := c.Update(ctx, dup, driver.Object{{Key: "a", Value: driver.Int(2)}}, nil); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("no changes: %v, want ErrSyntax", err)
	}
	if _, err := c.Insert(ctx, driver.CollectionRef{Name: "v"}, driver.Object{{Key: "a", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("insert into a view: %v, want ErrUnsupported", err)
	}

	people := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	key, err := c.Insert(ctx, people, driver.Object{{Key: "name", Value: driver.String("n")}, {Key: "email", Value: driver.String("gen@x")},
		{Key: "created_at", Value: driver.Time(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))}, {Key: "active", Value: driver.Bool(true)}, {Key: "score", Value: driver.Decimal("1.25")}})
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := key.Get("id"); !ok || id.Type() != driver.TypeInt {
		t.Errorf("generated key = %v", key)
	}
	_, rows, _ := query(t, c, driver.Query{Text: "select active, created_at, score from tobira_people where email = 'gen@x'"})
	if len(rows) != 1 || !drivertest.EqualValue(driver.Bool(true), rows[0][0]) || rows[0][2].String() != "1.25" {
		t.Errorf("inserted row = %v", rows)
	}
	if ts, ok := rows[0][1].AsTime(); !ok || !ts.Equal(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)) {
		t.Errorf("created_at = %v", rows[0][1])
	}
	if _, err := c.Insert(ctx, people, driver.Object{{Key: "name", Value: driver.String("n")}, {Key: "email", Value: driver.String("gen@x")}}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("duplicate email: %v, want ErrConstraint", err)
	}

	ro := f.open(t, true)
	if err := ro.Delete(ctx, dup, driver.Object{{Key: "a", Value: driver.Int(2)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only delete: %v, want ErrReadOnly", err)
	}
}

func TestCatalog(t *testing.T) {
	f := newFakeD1(t)
	f.mustExec(t, "create table _cf_KV (k, v)")
	f.mustExec(t, "create table parent (id integer primary key)")
	f.mustExec(t, "create table child (id integer primary key desc, p integer references parent(id), q text, unique (p, q))")
	f.mustExec(t, "create index child_q on child (q desc)")
	c := f.open(t, false)
	ctx := context.Background()

	nss, err := c.Namespaces(ctx)
	if err != nil || len(nss) != 1 || nss[0].Name != "main" || !nss[0].Default {
		t.Fatalf("Namespaces = %+v, %v", nss, err)
	}
	colls, err := c.Collections(ctx, driver.NamespaceRef{})
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range colls {
		if strings.HasPrefix(col.Ref.Name, "_cf_") || strings.HasPrefix(col.Ref.Name, "sqlite_") {
			t.Errorf("Collections lists internal table %q", col.Ref.Name)
		}
	}
	if _, err := c.Collections(ctx, driver.NamespaceRef{Name: "temp"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections(temp): %v, want ErrNotFound", err)
	}
	if _, err := c.Describe(ctx, driver.CollectionRef{Namespace: "other", Name: "child"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Describe(other.child): %v, want ErrNotFound", err)
	}
	s, err := c.Describe(ctx, driver.CollectionRef{Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Fields) != 3 || s.Fields[0].AutoIncrement || len(s.PrimaryKey) != 1 {
		t.Errorf("child fields = %+v (INTEGER PRIMARY KEY DESC is not a rowid alias)", s.Fields)
	}
	if len(s.ForeignKeys) != 1 || s.ForeignKeys[0].References.Name != "parent" || s.ForeignKeys[0].RefFields[0] != "id" {
		t.Errorf("foreign keys = %+v", s.ForeignKeys)
	}
	var sawQ, sawUnique bool
	for _, ix := range s.Indexes {
		sawQ = sawQ || ix.Name == "child_q" && len(ix.Fields) == 1 && ix.Fields[0] == "q"
		sawUnique = sawUnique || ix.Unique && len(ix.Fields) == 2
	}
	if !sawQ || !sawUnique {
		t.Errorf("indexes = %+v", s.Indexes)
	}
	if !strings.Contains(s.DDL, "primary key desc") {
		t.Errorf("DDL = %q", s.DDL)
	}
	// Identifiers with quotes reach D1 escaped, never as SQL.
	f.mustExec(t, `create table "we""ird" (a)`)
	if s, err := c.Describe(ctx, driver.CollectionRef{Name: `we"ird`}); err != nil || len(s.Fields) != 1 {
		t.Errorf(`Describe(we"ird) = %+v, %v`, s, err)
	}
}

func TestExplain(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, true)
	ctx := context.Background()
	p, err := c.Explain(ctx, driver.Query{Text: "select * from tobira_people where id = ?", Params: []driver.Value{driver.Int(1)}}, driver.ExplainOptions{})
	if err != nil || p.Text == "" || len(p.Root.Children) == 0 {
		t.Fatalf("Explain = %+v, %v", p, err)
	}
	before := len(f.log())
	for _, q := range []string{"pragma user_version = 3", "attach 'x' as y", "create table z (a)", "select 1; select 2"} {
		if _, err := c.Explain(ctx, driver.Query{Text: q}, driver.ExplainOptions{}); err == nil {
			t.Errorf("Explain(%q) passed", q)
		}
	}
	if _, err := c.Explain(ctx, driver.Query{Text: "select 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Analyze: %v, want ErrUnsupported", err)
	}
	if n := len(f.log()) - before; n != 0 {
		t.Errorf("%d refused explains were sent", n)
	}
	if p, err := c.Explain(ctx, driver.Query{Text: "delete from tobira_people"}, driver.ExplainOptions{}); err != nil || p.Text == "" {
		t.Errorf("Explain(delete) = %+v, %v", p, err)
	}
	if n := f.count(t, "select count(*) from tobira_people"); n != 57 {
		t.Errorf("explaining a delete deleted rows: %d left", n)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeEnvelope(w, http.StatusOK, []any{})
	}))
	defer other.Close()
	f := newFakeD1(t)
	c := f.open(t, false)
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
		return true
	})
	if _, err := c.Query(context.Background(), driver.Query{Text: "select 1"}); err == nil {
		t.Error("a redirect was treated as success")
	}
	if hits.Load() != 0 {
		t.Error("the redirect was followed (the token went to another server)")
	}
}

func TestResponseCapAndBadBodies(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, false)
	c.client.maxBytes = 2048
	if _, err := c.Query(context.Background(), driver.Query{Text: "select * from tobira_people"}); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("response over the cap: %v, want ErrLimit", err)
	}
	c.client.maxBytes = maxResponseBytes
	for name, body := range map[string]string{
		"html":           "<html>bad gateway</html>",
		"no result":      `{"result":[],"success":true,"errors":[]}`,
		"two results":    string(rawBody(`["a"]`, `[[1]]`))[:0] + `{"result":[{"results":{"columns":[],"rows":[]}},{"results":{"columns":[],"rows":[]}}],"success":true}`,
		"success, error": `{"result":[{"results":{"columns":["a"],"rows":[[1]]}}],"success":false,"errors":[{"code":7500,"message":"boom"}]}`,
		"hostile value":  string(rawBody(`["a"]`, `[[{"x":[[[]]]}]]`)),
	} {
		f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return true
		})
		if _, err := c.Query(context.Background(), driver.Query{Text: "select 1"}); err == nil {
			t.Errorf("%s: no error", name)
		} else {
			var de *driver.Error
			if !errors.As(err, &de) || de.Driver != Name {
				t.Errorf("%s: %v is not a d1 driver.Error", name, err)
			}
		}
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   int
		msg    string
		want   error
	}{
		{401, 10000, "Authentication error", driver.ErrAuth},
		{403, 10000, "Authentication error", driver.ErrAuth},
		{400, 6003, "Invalid request headers", driver.ErrAuth},
		{403, 7403, "The given account is not valid or is not authorized to access this service", driver.ErrPermission},
		{404, 7404, "The database x could not be found", driver.ErrNotFound},
		{429, 971, "Please wait and consider throttling your request speed", driver.ErrConnection},
		{502, 0, "bad gateway", driver.ErrConnection},
		{400, 7500, "no such table: nope: SQLITE_ERROR", driver.ErrNotFound},
		{400, 7500, "near \"selec\": syntax error: SQLITE_ERROR", driver.ErrSyntax},
		{400, 7500, "UNIQUE constraint failed: t.a: SQLITE_CONSTRAINT", driver.ErrConstraint},
		{400, 7500, "not authorized: SQLITE_AUTH", driver.ErrPermission},
		{400, 7500, "attempt to write a readonly database: SQLITE_READONLY", driver.ErrReadOnly},
		{400, 7500, "D1 DB is overloaded. Requests queued for too long.", driver.ErrConnection},
	} {
		err := mapError("query", &apiError{Status: tc.status, Code: tc.code, Message: tc.msg})
		if !errors.Is(err, tc.want) {
			t.Errorf("%d/%d %q: %v, want %v", tc.status, tc.code, tc.msg, err, tc.want)
		}
	}
	if err := mapError("query", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestCloseCancelsInFlight(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, false)
	started := make(chan struct{})
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		close(started)
		<-r.Context().Done()
		return true
	})
	done := make(chan error, 1)
	go func() {
		_, err := c.Query(context.Background(), driver.Query{Text: "select 1"})
		done <- err
	}()
	<-started
	_ = c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("the in-flight query succeeded after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the in-flight request")
	}
	if err := c.Ping(context.Background()); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Ping after Close: %v, want ErrConnection", err)
	}
}

// A write whose meta has no "changes" must not be reported as
// "no row matches the key": the statement may have run.
func TestMissingChangesIsConnectionError(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, false)
	ctx := context.Background()
	people := driver.CollectionRef{Name: "tobira_people"}
	if _, err := c.Describe(ctx, people); err != nil {
		t.Fatal(err)
	}
	f.setHook(func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if !strings.Contains(string(body), "UPDATE") && !strings.Contains(string(body), "DELETE") {
			return false
		}
		writeEnvelope(w, http.StatusOK, []any{map[string]any{
			"results": map[string]any{"columns": []string{}, "rows": [][]any{}}, "success": true, "meta": map[string]any{"duration": 1},
		}})
		return true
	})
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	if err := c.Update(ctx, people, key, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("update without meta.changes: %v, want ErrConnection", err)
	}
	if err := c.Delete(ctx, people, key); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("delete without meta.changes: %v, want ErrConnection", err)
	}
}

// A server (or proxy) echoing the Authorization header never puts
// the token into an error after Open either.
func TestErrorsAfterOpenMaskToken(t *testing.T) {
	f := newFakeD1(t)
	c := f.open(t, false)
	for name, h := range map[string]func(w http.ResponseWriter, r *http.Request){
		"envelope": func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(w, http.StatusBadRequest, nil, cfError{7500, "bad: " + r.Header.Get("Authorization")})
		},
		"plain": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("proxy saw " + r.Header.Get("Authorization")))
		},
	} {
		f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool { h(w, r); return true })
		_, err := c.Query(context.Background(), driver.Query{Text: "select 1"})
		if err == nil || strings.Contains(err.Error(), fakeToken) {
			t.Errorf("%s: error leaks the token or is nil: %v", name, err)
		}
	}
}

func TestCloudflareCodesMapped(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{
		{7403, driver.ErrPermission},
		{9109, driver.ErrPermission},
		{7404, driver.ErrNotFound},
		{7003, driver.ErrNotFound},
		{7000, driver.ErrNotFound},
		{7400, driver.ErrSyntax},
		{971, driver.ErrConnection},
		{10000, driver.ErrAuth},
	} {
		if err := mapError("query", &apiError{Status: 400, Code: tc.code, Message: "x"}); !errors.Is(err, tc.want) {
			t.Errorf("400 with code %d: %v, want %v", tc.code, err, tc.want)
		}
	}
}
