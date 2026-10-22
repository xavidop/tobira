package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/xavidop/tobira/driver"
)

var peopleRef = driver.CollectionRef{Namespace: "main", Name: "tobira_people"}

func browseAll(t *testing.T, c driver.Conn, opts driver.BrowseOptions) ([]driver.Row, driver.Stats, error) {
	t.Helper()
	rs, err := c.(driver.Browser).Browse(context.Background(), peopleRef, opts)
	if err != nil {
		return nil, driver.Stats{}, err
	}
	return driver.Collect(context.Background(), rs, 0)
}

func TestBrowseErrors(t *testing.T) {
	c := openTemp(t, false)
	cases := map[string]struct {
		opts driver.BrowseOptions
		want error
	}{
		"bad cursor":    {driver.BrowseOptions{Cursor: "x"}, driver.ErrSyntax},
		"unknown field": {driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "nope", Op: driver.OpEq, Value: driver.Int(1)}}}}, driver.ErrNotFound},
		"unknown sort":  {driver.BrowseOptions{Sort: []driver.SortKey{{Field: "nope"}}}, driver.ErrNotFound},
		"unknown op":    {driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: "zz", Value: driver.Int(1)}}}}, driver.ErrUnsupported},
		"in non-list":   {driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.Int(1)}}}}, driver.ErrSyntax},
		"vector":        {driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpEq, Value: driver.Vector([]float32{1})}}}}, driver.ErrUnsupported},
		"raw ro attach": {driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: "1); ATTACH 'x.db' AS x; SELECT (1"}}, driver.ErrSyntax},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := browseAll(t, c, tc.opts); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBrowseReadOnlyConn(t *testing.T) {
	c := openTemp(t, true)
	_, _, err := browseAll(t, c, driver.BrowseOptions{Filter: driver.Filter{Raw: "id in (select 1)"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBrowseLikeEscapes(t *testing.T) {
	c := openTemp(t, false)
	rows, _, err := browseAll(t, c, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "name", Op: driver.OpContains, Value: driver.String("%")}}}})
	if err != nil || len(rows) != 0 {
		t.Fatalf("contains %% matched %d rows, err %v", len(rows), err)
	}
}

func TestExplain(t *testing.T) {
	c := openTemp(t, false)
	e := c.(driver.Explainer)
	p, err := e.Explain(context.Background(), driver.Query{Text: "select * from tobira_people where id = 1"}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != "text" || p.Text == "" || p.Root == nil || len(p.Root.Children) == 0 {
		t.Fatalf("plan = %+v", p)
	}
	if _, err := e.Explain(context.Background(), driver.Query{Text: "select 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("analyze err = %v", err)
	}
	_, err = e.Explain(context.Background(), driver.Query{Text: "ATTACH 'x.db' AS x", ReadOnly: true}, driver.ExplainOptions{})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("ro attach err = %v", err)
	}
}

func TestBrowseNullEquality(t *testing.T) {
	c := openTemp(t, false)
	cond := func(op driver.Op) driver.BrowseOptions {
		return driver.BrowseOptions{Limit: 1000, Filter: driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: op, Value: driver.Null()}}}}
	}
	nulls, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people where age is null"})
	all, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people"})
	if len(nulls) == 0 {
		t.Fatal("dataset has no NULL ages")
	}
	if rows, _, err := browseAll(t, c, cond(driver.OpEq)); err != nil || len(rows) != len(nulls) {
		t.Fatalf("eq null: %d rows, want %d (err %v)", len(rows), len(nulls), err)
	}
	if rows, _, err := browseAll(t, c, cond(driver.OpNe)); err != nil || len(rows) != len(all)-len(nulls) {
		t.Fatalf("ne null: %d rows, want %d (err %v)", len(rows), len(all)-len(nulls), err)
	}
}

func userVersion(t *testing.T, c driver.Conn) int64 {
	t.Helper()
	rows, _, _ := run(t, c, driver.Query{Text: "PRAGMA user_version"})
	v, _ := rows[0][0].AsInt()
	return v
}

func TestExplainNeverWrites(t *testing.T) {
	c := openTemp(t, false)
	e := c.(driver.Explainer)
	ctx := context.Background()
	for _, text := range []string{
		"delete from tobira_people where age > 0",
		"update tobira_people set age = 1 where name like '%a%'",
		"insert into tobira_people (id, name, email) select 999, 'x', 'x@x' from tobira_people where id = 1",
		"replace into tobira_people (id, name, email) select 1, 'x', 'x@x' from tobira_people where id = 1",
		"with d as (select 1) delete from tobira_people where age > 0",
	} {
		p, err := e.Explain(ctx, driver.Query{Text: text}, driver.ExplainOptions{})
		if err != nil || p.Text == "" {
			t.Fatalf("explain %q: plan %+v err %v", text, p, err)
		}
	}
	if rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people where id = 999 or name = 'x'"}); len(rows) != 0 {
		t.Fatalf("explain changed data: %d rows", len(rows))
	}
	if rows, _, _ := run(t, c, driver.Query{Text: "select * from tobira_people"}); len(rows) != 57 {
		t.Fatalf("explain deleted rows: %d left", len(rows))
	}

	for _, text := range []string{"PRAGMA user_version=5", "PRAGMA user_version = 5;", "ATTACH 'x.db' AS x", "VACUUM", "EXPLAIN delete from tobira_people", "create table z (a)"} {
		_, err := e.Explain(ctx, driver.Query{Text: text}, driver.ExplainOptions{})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("explain %q on a writable conn: err = %v, want ErrReadOnly", text, err)
		}
	}
	if v := userVersion(t, c); v != 0 {
		t.Fatalf("user_version = %d after explain, want 0", v)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(c.(*conn).path), "x.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("explain of ATTACH created x.db (stat err %v)", err)
	}
}

func TestExplainWriteOnReadOnlyConn(t *testing.T) {
	c := openTemp(t, true)
	p, err := c.(driver.Explainer).Explain(context.Background(), driver.Query{Text: "delete from tobira_people where age > 1"}, driver.ExplainOptions{})
	if err != nil || p.Text == "" {
		t.Fatalf("plan %+v err %v", p, err)
	}
}

func TestExplainDirectWrite(t *testing.T) {
	c := openTemp(t, false)
	e := c.(driver.Explainer)
	for _, text := range []string{
		"insert into tobira_people (id, name, email) values (999, 'x', 'x@x')",
		"delete from tobira_people",
	} {
		p, err := e.Explain(context.Background(), driver.Query{Text: text}, driver.ExplainOptions{})
		if err != nil {
			t.Fatalf("explain %q: %v", text, err)
		}
		if p.Text != noPlanRowsWrite {
			t.Errorf("explain %q: Text = %q, want %q", text, p.Text, noPlanRowsWrite)
		}
	}
	for _, text := range []string{"PRAGMA table_info(tobira_people)", "PRAGMA user_version"} {
		p, err := e.Explain(context.Background(), driver.Query{Text: text}, driver.ExplainOptions{})
		if err != nil {
			t.Fatalf("explain %q: %v", text, err)
		}
		if p.Text == noPlanRowsWrite {
			t.Errorf("explain %q: a read-only PRAGMA is not a direct write: %q", text, p.Text)
		}
	}
	p, err := e.Explain(context.Background(), driver.Query{Text: "select * from tobira_people where id = 1"}, driver.ExplainOptions{})
	if err != nil || p.Text == noPlanRows || p.Text == noPlanRowsWrite {
		t.Errorf("a SELECT must keep its plan text: %+v, %v", p, err)
	}
}

func TestIsWriteStatementWith(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"with c as (select 1) insert into t select * from c", true},
		{"WITH c AS (SELECT 1) update t set a = 1", true},
		{"with c as (select 1) delete from t", true},
		{"with c as (select 1) replace into t values (1)", true},
		{"with c as (select updated_at from t) select * from c", false},
		{"with c as (select 1 as inserted, deleted_at from t) select * from c", false},
		{"with c as (select 'delete me') select * from c", false},
		{"with c as (select \"update\" from t) select * from c", false},
		{"with c as (select `insert` from t) select * from c", false},
		{"with c as (select [replace] from t) select * from c", false},
		{"with c as (select 1) -- update later\nselect * from c", false},
		{"with c as (select 1 /* delete */) select * from c", false},
		{"with c as (select 'it''s' as x) delete from t", true},
		{"with c as (select 1) select 'a' || \"b\"\"c\" from c", false},
		{"with c as (select 1) select updaté from c", false},
		{"insert into t values (1)", true},
		{"select 1", false},
	}
	for _, tc := range tests {
		if got := isWriteStatement(tc.text); got != tc.want {
			t.Errorf("isWriteStatement(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestRowidTiebreak(t *testing.T) {
	fields := func(names ...string) *driver.CollectionSchema {
		s := &driver.CollectionSchema{Type: driver.CollectionTable}
		for _, n := range names {
			s.Fields = append(s.Fields, driver.Field{Name: n})
		}
		return s
	}
	tests := []struct {
		name   string
		fields []string
		want   string
	}{
		{"plain table", []string{"a", "b"}, "rowid"},
		{"rowid shadowed", []string{"rowid", "b"}, "_rowid_"},
		{"rowid and _rowid_ shadowed", []string{"RowID", "_ROWID_"}, "oid"},
		{"all shadowed", []string{"rowid", "_rowid_", "oid"}, ""},
	}
	for _, tt := range tests {
		if got := rowidTiebreak(fields(tt.fields...)); got != tt.want {
			t.Errorf("%s: rowidTiebreak = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestBrowseShadowedRowid is a smoke test: SQLite scans in rowid order anyway,
// so it cannot tell the tiebreak apart; TestRowidTiebreak is the guard.
func TestBrowseShadowedRowid(t *testing.T) {
	c := openTemp(t, false)
	ctx := context.Background()
	for _, q := range []string{
		"create table shadow (rowid integer, v text)",
		"create table shadow_all (rowid integer, _rowid_ integer, oid integer, v text)",
	} {
		if _, err := c.Query(ctx, driver.Query{Text: q}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"shadow", "shadow_all"} {
		var n int
		for i := range 25 {
			text := "insert into " + name + " (v) values ('" + strconv.Itoa(i) + "')"
			if _, err := c.Query(ctx, driver.Query{Text: text}); err != nil {
				t.Fatal(err)
			}
			n++
		}
		seen := map[string]bool{}
		cursor := ""
		for pages := 0; pages < 10; pages++ {
			rs, err := c.(driver.Browser).Browse(ctx, driver.CollectionRef{Namespace: "main", Name: name}, driver.BrowseOptions{Limit: 10, Cursor: cursor})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			rows, st, err := driver.Collect(ctx, rs, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				seen[r[len(r)-1].String()] = true
			}
			if cursor = st.NextCursor; cursor == "" {
				break
			}
		}
		if len(seen) != n {
			t.Errorf("%s: paging saw %d of %d rows", name, len(seen), n)
		}
	}
}
