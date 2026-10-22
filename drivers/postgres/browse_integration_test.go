//go:build integration

package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

var people = driver.CollectionRef{Namespace: "public", Name: "tobira_people"}

// browseAll pages through ref until NextCursor is empty and returns the pages.
func browseAll(t *testing.T, c *conn, ref driver.CollectionRef, opts driver.BrowseOptions) (pages [][]driver.Row, cursors []string, cols []driver.Column) {
	t.Helper()
	for i := 0; i < 50; i++ {
		rs, err := c.Browse(ctxT(t), ref, opts)
		if err != nil {
			t.Fatalf("Browse(page %d): %v", i+1, err)
		}
		cols = rs.Columns()
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatalf("Browse(page %d) rows: %v", i+1, err)
		}
		pages = append(pages, rows)
		if st.NextCursor == "" {
			if st.Truncated {
				t.Fatal("Truncated without NextCursor")
			}
			return
		}
		if !st.Truncated {
			t.Fatal("NextCursor without Truncated")
		}
		cursors = append(cursors, st.NextCursor)
		opts.Cursor = st.NextCursor
	}
	t.Fatal("never reached the last page")
	return
}

func colIdx(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no column %q", name)
	}
	return i
}

func idsOf(t *testing.T, pages [][]driver.Row, cols []driver.Column, name string) []string {
	t.Helper()
	i := colIdx(t, cols, name)
	var out []string
	for _, p := range pages {
		for _, r := range p {
			out = append(out, r[i].String())
		}
	}
	return out
}

func TestBrowseKeysetPaging(t *testing.T) {
	c := openPG(t, false)
	pages, cursors, cols := browseAll(t, c, people, driver.BrowseOptions{Limit: 20})
	if len(pages) != 3 || len(pages[0]) != 20 || len(pages[2]) != 17 {
		t.Fatalf("pages = %d (first %d)", len(pages), len(pages[0]))
	}
	for _, cur := range cursors {
		if !strings.HasPrefix(cur, "k:") {
			t.Errorf("cursor %q is not a keyset cursor", cur)
		}
	}
	var prev int64
	for _, id := range idsOf(t, pages, cols, "id") {
		n, _ := strconv.ParseInt(id, 10, 64)
		if n <= prev {
			t.Fatalf("id %d follows %d: keyset pages overlap or are out of order", n, prev)
		}
		prev = n
	}
	if got := len(idsOf(t, pages, cols, "id")); got != 57 {
		t.Errorf("got %d ids, want 57", got)
	}
}

func TestBrowseKeysetAfterDeletion(t *testing.T) {
	// A keyset page continues after the last key even if that row vanished.
	c := openPG(t, false)
	tbl := scratch(t, c)
	mustExec(t, c, "ALTER TABLE "+tbl+" ADD PRIMARY KEY (id)")
	ref := driver.CollectionRef{Namespace: "public", Name: tbl}
	rs, err := c.Browse(ctxT(t), ref, driver.BrowseOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	_, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "DELETE FROM "+tbl+" WHERE id <= 5")
	pages, _, cols := browseAll(t, c, ref, driver.BrowseOptions{Limit: 100, Cursor: st.NextCursor})
	ids := idsOf(t, pages, cols, "id")
	if len(ids) != 52 {
		t.Errorf("got %d rows after the cursor, want 52", len(ids))
	}
}

func TestBrowseCompositeAndTypedKeys(t *testing.T) {
	c := openPG(t, false)
	mustExec(t, c, "DROP TABLE IF EXISTS tobira_keys")
	mustExec(t, c, "CREATE TABLE tobira_keys (a integer, b text, at timestamptz, n numeric(8,2), u uuid, PRIMARY KEY (a, b))")
	mustExec(t, c, "INSERT INTO tobira_keys SELECT g/3, 'k' || (g%3), '2026-01-01 00:00:00.123456+00'::timestamptz + g * interval '1 second', g::numeric / 4, gen_random_uuid() FROM generate_series(0, 10) g")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS tobira_keys") })
	ref := driver.CollectionRef{Namespace: "public", Name: "tobira_keys"}
	pages, _, cols := browseAll(t, c, ref, driver.BrowseOptions{Limit: 2})
	a, b := idsOf(t, pages, cols, "a"), idsOf(t, pages, cols, "b")
	if len(a) != 11 {
		t.Fatalf("got %d rows, want 11", len(a))
	}
	for i := range a {
		if want := fmt.Sprintf("%d/k%d", i/3, i%3); a[i]+"/"+b[i] != want {
			t.Fatalf("row %d = %s/%s, want %s", i, a[i], b[i], want)
		}
	}

	// Timestamp, numeric and uuid primary keys round-trip through the cursor.
	for _, key := range []string{"at", "n", "u"} {
		mustExec(t, c, "ALTER TABLE tobira_keys DROP CONSTRAINT tobira_keys_pkey")
		mustExec(t, c, "ALTER TABLE tobira_keys ADD PRIMARY KEY ("+key+")")
		pages, _, cols := browseAll(t, c, ref, driver.BrowseOptions{Limit: 3})
		if got := idsOf(t, pages, cols, "a"); len(got) != 11 {
			t.Errorf("pk %s: got %d rows, want 11", key, len(got))
		}
	}
}

func TestBrowseOffsetPaging(t *testing.T) {
	c := openPG(t, false)
	// An explicit sort pages by offset, even with a primary key.
	pages, cursors, cols := browseAll(t, c, people, driver.BrowseOptions{Limit: 25, Sort: []driver.SortKey{{Field: "age", Desc: true}}})
	if len(cursors) != 2 || cursors[0] != "o:25" || cursors[1] != "o:50" {
		t.Errorf("cursors = %v", cursors)
	}
	ids := idsOf(t, pages, cols, "id")
	if len(slices.Compact(slices.Sorted(slices.Values(ids)))) != 57 {
		t.Errorf("pages overlap or lose rows: %d ids", len(ids))
	}
	ai := colIdx(t, cols, "age")
	if first := pages[0][0][ai]; first.IsNull() {
		// desc puts NULLs first
		last := pages[2][len(pages[2])-1][ai]
		if last.IsNull() {
			t.Error("NULLs on both ends")
		}
	}

	// No primary key: ctid keeps paging stable. A view has no ctid and no sort.
	tbl := scratch(t, c)
	pages, cursors, _ = browseAll(t, c, driver.CollectionRef{Namespace: "public", Name: tbl}, driver.BrowseOptions{Limit: 20})
	if len(pages) != 3 || !strings.HasPrefix(cursors[0], "o:") {
		t.Errorf("no-pk table: %d pages, cursors %v", len(pages), cursors)
	}
	pages, _, _ = browseAll(t, c, driver.CollectionRef{Namespace: "public", Name: "tobira_people_view"}, driver.BrowseOptions{Limit: 30})
	if len(pages) != 2 {
		t.Errorf("view: %d pages, want 2", len(pages))
	}
	pages, _, _ = browseAll(t, c, driver.CollectionRef{Namespace: "public", Name: "tobira_people_mv"}, driver.BrowseOptions{Limit: 30})
	if len(pages) != 2 {
		t.Errorf("materialized view: %d pages, want 2", len(pages))
	}
}

func TestBrowseBadCursors(t *testing.T) {
	c := openPG(t, false)
	for _, cur := range []string{"junk", "k:!!!", "k:AAAA", "o:5", "k:" + "WzEsMl0"} { // o:5 is the wrong mode for keyset
		_, err := c.Browse(ctxT(t), people, driver.BrowseOptions{Cursor: cur})
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("cursor %q: want ErrSyntax, got %v", cur, err)
		}
	}
	_, err := c.Browse(ctxT(t), people, driver.BrowseOptions{Cursor: "k:WzVd", Sort: []driver.SortKey{{Field: "id"}}})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("keyset cursor on a sorted browse: want ErrSyntax, got %v", err)
	}
	requireIdle(t, c)
}

func mustBrowse(t *testing.T, c *conn, ref driver.CollectionRef, opts driver.BrowseOptions) driver.ResultSet {
	t.Helper()
	rs, err := c.Browse(ctxT(t), ref, opts)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	return rs
}

func browseIDs(t *testing.T, c *conn, ref driver.CollectionRef, f driver.Filter) []string {
	t.Helper()
	pages, _, cols := browseAll(t, c, ref, driver.BrowseOptions{Limit: 500, Filter: f})
	return idsOf(t, pages, cols, "id")
}

func TestBrowseFilters(t *testing.T) {
	c := openPG(t, false)
	cond := func(field string, op driver.Op, v driver.Value) driver.Filter {
		return driver.Filter{Conditions: []driver.Condition{{Field: field, Op: op, Value: v}}}
	}
	n := func(f driver.Filter) int { return len(browseIDs(t, c, people, f)) }

	total := n(driver.Filter{})
	if total != 57 {
		t.Fatalf("total = %d", total)
	}
	if got := n(cond("id", driver.OpLt, driver.Int(10))); got != 9 {
		t.Errorf("id < 10 = %d", got)
	}
	if got := n(cond("id", driver.OpLe, driver.Int(10))); got != 10 {
		t.Errorf("id <= 10 = %d", got)
	}
	if got := n(cond("id", driver.OpGt, driver.Int(50))); got != 7 {
		t.Errorf("id > 50 = %d", got)
	}
	if got := n(cond("id", driver.OpGe, driver.Int(50))); got != 8 {
		t.Errorf("id >= 50 = %d", got)
	}
	if got := n(cond("id", driver.OpEq, driver.Int(7))); got != 1 {
		t.Errorf("id = 7 = %d", got)
	}
	if got := n(cond("id", driver.OpNe, driver.Int(7))); got != 56 {
		t.Errorf("id <> 7 = %d", got)
	}
	if got := n(cond("id", driver.OpIn, driver.List(driver.Int(1), driver.Int(2), driver.Int(99)))); got != 2 {
		t.Errorf("id in = %d", got)
	}
	if got := n(cond("id", driver.OpIn, driver.List())); got != 0 {
		t.Errorf("id in () = %d", got)
	}
	nulls, notNulls := n(cond("age", driver.OpIsNull, driver.Value{})), n(cond("age", driver.OpNotNull, driver.Value{}))
	if nulls == 0 || nulls+notNulls != total {
		t.Errorf("is_null %d + not_null %d != %d", nulls, notNulls, total)
	}
	if got := n(cond("age", driver.OpEq, driver.Null())); got != nulls {
		t.Errorf("age eq null = %d, want %d", got, nulls)
	}
	if got := n(cond("age", driver.OpNe, driver.Null())); got != notNulls {
		t.Errorf("age ne null = %d, want %d", got, notNulls)
	}
	// Case-insensitive field match and AND of conditions.
	both := driver.Filter{Conditions: []driver.Condition{
		{Field: "ID", Op: driver.OpGt, Value: driver.Int(10)}, {Field: "id", Op: driver.OpLt, Value: driver.Int(20)},
	}}
	if got := n(both); got != 9 {
		t.Errorf("10 < id < 20 = %d", got)
	}
	// Raw is parenthesised, so an OR inside it cannot escape the other filters.
	raw := driver.Filter{Raw: "id = 1 OR id = 2", Conditions: []driver.Condition{{Field: "id", Op: driver.OpGt, Value: driver.Int(1)}}}
	if got := browseIDs(t, c, people, raw); !slices.Equal(got, []string{"2"}) {
		t.Errorf("raw+condition = %v", got)
	}
	// Raw cannot smuggle a second statement.
	_, err := c.Browse(ctxT(t), people, driver.BrowseOptions{Filter: driver.Filter{Raw: "true); DROP TABLE tobira_people; --"}})
	if err == nil {
		t.Error("raw with a second statement accepted")
	}
	if got := count(t, c, "tobira_people"); got != 57 {
		t.Errorf("tobira_people has %d rows after the raw attempt", got)
	}
}

func TestBrowseLikeEscaping(t *testing.T) {
	c := openPG(t, false)
	mustExec(t, c, "DROP TABLE IF EXISTS tobira_like")
	mustExec(t, c, `CREATE TABLE tobira_like (id integer PRIMARY KEY, v text)`)
	mustExec(t, c, `INSERT INTO tobira_like VALUES (1, '50% off'), (2, '500 off'), (3, 'a_b'), (4, 'axb'), (5, 'back\slash'), (6, '50%')`)
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS tobira_like") })
	ref := driver.CollectionRef{Namespace: "public", Name: "tobira_like"}
	f := func(op driver.Op, v string) []string {
		return browseIDs(t, c, ref, driver.Filter{Conditions: []driver.Condition{{Field: "v", Op: op, Value: driver.String(v)}}})
	}
	for _, tc := range []struct {
		op   driver.Op
		v    string
		want []string
	}{
		{driver.OpContains, "50%", []string{"1", "6"}},
		{driver.OpContains, "a_b", []string{"3"}},
		{driver.OpContains, `\`, []string{"5"}},
		{driver.OpPrefix, "50%", []string{"1", "6"}},
		{driver.OpPrefix, "a_", []string{"3"}},
		{driver.OpContains, "OFF", nil},
	} {
		if got := f(tc.op, tc.v); !slices.Equal(got, tc.want) {
			t.Errorf("%s %q = %v, want %v", tc.op, tc.v, got, tc.want)
		}
	}
	// contains works on a non-text column (cast to text).
	if got := browseIDs(t, c, ref, driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpContains, Value: driver.String("3")}}}); !slices.Equal(got, []string{"3"}) {
		t.Errorf("id contains 3 = %v", got)
	}
}

func TestBrowseIdentifiersAndErrors(t *testing.T) {
	c := openPG(t, false)
	mustExec(t, c, `DROP TABLE IF EXISTS "Tobira Odd"`)
	mustExec(t, c, `CREATE TABLE "Tobira Odd" ("Id" integer PRIMARY KEY, "Mixed ""Q""" text, ab text, "AB" text)`)
	mustExec(t, c, `INSERT INTO "Tobira Odd" VALUES (1, 'x', 'lower', 'upper'), (2, 'y', 'l2', 'u2')`)
	t.Cleanup(func() { mustExec(t, c, `DROP TABLE IF EXISTS "Tobira Odd"`) })
	ref := driver.CollectionRef{Namespace: "public", Name: "Tobira Odd"}

	pages, _, cols := browseAll(t, c, ref, driver.BrowseOptions{
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: `Mixed "Q"`, Op: driver.OpEq, Value: driver.String("y")}}},
		Sort:   []driver.SortKey{{Field: "ID", Desc: true}},
	})
	if got := idsOf(t, pages, cols, "Id"); !slices.Equal(got, []string{"2"}) {
		t.Errorf("odd identifiers = %v", got)
	}
	// "ab" and "AB" are both exact matches; "Ab" is ambiguous. Ascending, the
	// first row of each column is its smallest value.
	for field, want := range map[string]string{"ab": "l2", "AB": "u2"} {
		rs, err := c.Browse(ctxT(t), ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: field}}, Limit: 1})
		if err != nil {
			t.Fatalf("sort %s: %v", field, err)
		}
		rows, _, _ := driver.Collect(ctxT(t), rs, 0)
		if rows[0][colIdx(t, rs.Columns(), field)].String() != want {
			t.Errorf("sort %s picked the wrong column", field)
		}
	}
	for name, opts := range map[string]driver.BrowseOptions{
		"ambiguous filter": {Filter: driver.Filter{Conditions: []driver.Condition{{Field: "Ab", Op: driver.OpIsNull}}}},
		"unknown filter":   {Filter: driver.Filter{Conditions: []driver.Condition{{Field: "nope", Op: driver.OpIsNull}}}},
		"unknown sort":     {Sort: []driver.SortKey{{Field: "nope"}}},
		"injected sort":    {Sort: []driver.SortKey{{Field: `id"; DROP TABLE tobira_people; --`}}},
	} {
		if _, err := c.Browse(ctxT(t), ref, opts); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", name, err)
		}
	}
	if _, err := c.Browse(ctxT(t), ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "Id", Op: "regex"}}}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown op: want ErrUnsupported, got %v", err)
	}
	if _, err := c.Browse(ctxT(t), driver.CollectionRef{Namespace: "public", Name: "nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing collection: want ErrNotFound, got %v", err)
	}
	if got := count(t, c, "tobira_people"); got != 57 {
		t.Errorf("tobira_people damaged: %d rows", got)
	}
}

func TestBrowseReadOnly(t *testing.T) {
	ro := openPG(t, true)
	pages, _, _ := browseAll(t, ro, people, driver.BrowseOptions{Limit: 30})
	if len(pages) != 2 {
		t.Errorf("read-only conn: %d pages", len(pages))
	}
	// opts.ReadOnly on a writable conn runs Raw in a read-only transaction.
	rw := openPG(t, false)
	tbl := scratch(t, rw)
	ref := driver.CollectionRef{Namespace: "public", Name: tbl}
	raw := "id = (SELECT 1 FROM (SELECT set_config('x.y','1',false)) s LIMIT 1)"
	if _, _, err := driver.Collect(ctxT(t), mustBrowse(t, rw, ref, driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: raw}}), 0); err != nil {
		t.Errorf("read-only browse with harmless raw: %v", err)
	}
	mustExec(t, rw, "CREATE OR REPLACE FUNCTION tobira_poke() RETURNS integer LANGUAGE sql VOLATILE AS $$ INSERT INTO "+tbl+" (id) VALUES (9999) RETURNING 1 $$")
	t.Cleanup(func() { mustExec(t, rw, "DROP FUNCTION IF EXISTS tobira_poke()") })
	rs, err := rw.Browse(ctxT(t), ref, driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: "tobira_poke() = 1"}})
	if err == nil {
		_, _, err = driver.Collect(ctxT(t), rs, 0)
	}
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("side-effecting raw filter with ReadOnly: want ErrReadOnly, got %v", err)
	}
	if got := count(t, rw, tbl); got != 57 {
		t.Errorf("scratch table has %d rows, the raw filter wrote", got)
	}
}

func TestBrowseNoConnLeak(t *testing.T) {
	c := openPG(t, false)
	// Abandon a page half-read, then fail one; the pool must come back whole.
	rs, err := c.Browse(ctxT(t), people, driver.BrowseOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rs.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	_, _ = c.Browse(ctxT(t), people, driver.BrowseOptions{Filter: driver.Filter{Raw: "nope ="}})
	requireIdle(t, c)
}

func TestExplain(t *testing.T) {
	c := openPG(t, false)
	plan, err := c.Explain(ctxT(t), driver.Query{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(7)}}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Format != "text" || !strings.Contains(plan.Text, "tobira_people") || !strings.Contains(plan.Text, "\n") {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Root == nil || plan.Root.Label == "" || plan.Root.Rows < 0 || plan.Root.Cost <= 0 {
		t.Fatalf("root = %+v", plan.Root)
	}
	if !strings.Contains(plan.Root.Label+plan.Root.Detail, "Scan") || !strings.Contains(plan.Root.Detail, "tobira_people") {
		t.Errorf("root = %+v", plan.Root)
	}

	join, err := c.Explain(ctxT(t), driver.Query{Text: "select * from tobira_orders o join tobira_people p on p.id = o.person_id"}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(join.Root.Children) != 2 {
		t.Errorf("join root = %+v", join.Root)
	}

	// Explain only plans: a write is not executed.
	scratchT := scratch(t, c)
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "delete from " + scratchT}, driver.ExplainOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, c, scratchT); got != 57 {
		t.Errorf("EXPLAIN ran the delete: %d rows left", got)
	}

	if _, err := c.Explain(ctxT(t), driver.Query{Text: "select * from nope"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing table: want ErrNotFound, got %v", err)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "select 1; select 2"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("two statements: want ErrSyntax, got %v", err)
	}
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "select * from tobira_people", Params: []driver.Value{driver.Int(1)}}, driver.ExplainOptions{}); err == nil {
		t.Error("surplus parameter accepted")
	}
	// Read-only: planning still works, nothing leaks.
	ro := openPG(t, true)
	if _, err := ro.Explain(ctxT(t), driver.Query{Text: "delete from " + scratchT}, driver.ExplainOptions{}); err != nil {
		t.Errorf("planning a write on a read-only conn: %v", err)
	}
}

func TestExplainAnalyzeRollsBack(t *testing.T) {
	c := openPG(t, false)
	tbl := scratch(t, c)
	plan, err := c.Explain(ctxT(t), driver.Query{Text: "delete from " + tbl + " where id <= 10"}, driver.ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Format != "json" || !strings.Contains(plan.Text, "Actual Total Time") || plan.Root == nil {
		t.Errorf("analyze plan = %+v", plan)
	}
	if got := count(t, c, tbl); got != 57 {
		t.Errorf("EXPLAIN ANALYZE persisted its delete: %d rows left", got)
	}

	// A select analyzes fine and reports actual timings.
	if _, err := c.Explain(ctxT(t), driver.Query{Text: "select * from tobira_people"}, driver.ExplainOptions{Analyze: true}); err != nil {
		t.Errorf("analyze select: %v", err)
	}

	ro := openPG(t, true)
	_, err = ro.Explain(ctxT(t), driver.Query{Text: "delete from " + tbl}, driver.ExplainOptions{Analyze: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("analyze write on a read-only conn: want ErrReadOnly, got %v", err)
	}
	_, err = c.Explain(ctxT(t), driver.Query{Text: "delete from " + tbl, ReadOnly: true}, driver.ExplainOptions{Analyze: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("analyze write with q.ReadOnly: want ErrReadOnly, got %v", err)
	}
	if got := count(t, c, tbl); got != 57 {
		t.Errorf("rows changed: %d", got)
	}
	requireIdle(t, c)
}

func TestBrowseInvalidKeyCursorValues(t *testing.T) {
	c := openPG(t, false)
	mustExec(t, c, "DROP TABLE IF EXISTS tobira_badkeys; DROP TYPE IF EXISTS tobira_mood")
	mustExec(t, c, "CREATE TYPE tobira_mood AS ENUM ('ok', 'sad')")
	t.Cleanup(func() {
		mustExec(t, c, "DROP TABLE IF EXISTS tobira_badkeys; DROP TYPE IF EXISTS tobira_mood")
	})
	for _, keyType := range []string{"uuid", "numeric", "tobira_mood", "date", "integer"} {
		mustExec(t, c, "DROP TABLE IF EXISTS tobira_badkeys")
		mustExec(t, c, "CREATE TABLE tobira_badkeys (k "+keyType+" PRIMARY KEY)")
		ref := driver.CollectionRef{Namespace: "public", Name: "tobira_badkeys"}
		for _, key := range []string{`"zzz"`, `"abc"`, `"2026-99-99"`} {
			cur := "k:" + base64.RawURLEncoding.EncodeToString([]byte("["+key+"]"))
			rs, err := c.Browse(ctxT(t), ref, driver.BrowseOptions{Cursor: cur})
			if err == nil {
				_, _, err = driver.Collect(ctxT(t), rs, 0)
			}
			if !errors.Is(err, driver.ErrSyntax) {
				t.Errorf("%s key %s: want ErrSyntax, got %v", keyType, key, err)
			}
		}
	}
	requireIdle(t, c)
}

func TestBrowseRawCommentCannotDropLimit(t *testing.T) {
	c := openPG(t, false)
	for _, raw := range []string{"true -- trailing", "true) ORDER BY 1 --"} {
		rs, err := c.Browse(ctxT(t), people, driver.BrowseOptions{Limit: 10, Filter: driver.Filter{Raw: raw}})
		if err != nil {
			continue // an error is fine; an unbounded result is not
		}
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err == nil && (len(rows) > 10 || !st.Truncated) {
			t.Errorf("raw %q: %d rows, truncated %v", raw, len(rows), st.Truncated)
		}
	}
}

func TestBrowseInStringAndUUIDLists(t *testing.T) {
	c := openPG(t, false)
	mustExec(t, c, "DROP TABLE IF EXISTS tobira_inlist")
	mustExec(t, c, "CREATE TABLE tobira_inlist (id uuid PRIMARY KEY, name text)")
	mustExec(t, c, "INSERT INTO tobira_inlist VALUES ('00000000-0000-0000-0000-000000000001', 'a'), ('00000000-0000-0000-0000-000000000002', 'b'), ('00000000-0000-0000-0000-000000000003', 'c')")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS tobira_inlist") })
	ref := driver.CollectionRef{Namespace: "public", Name: "tobira_inlist"}
	names := func(field string, vals ...string) []string {
		list := make([]driver.Value, len(vals))
		for i, v := range vals {
			list[i] = driver.String(v)
		}
		pages, _, cols := browseAll(t, c, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: field, Op: driver.OpIn, Value: driver.List(list...)}}}})
		return idsOf(t, pages, cols, "name")
	}
	if got := names("name", "a", "c", "zz"); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("name in = %v", got)
	}
	if got := names("id", "00000000-0000-0000-0000-000000000002"); !slices.Equal(got, []string{"b"}) {
		t.Errorf("uuid in = %v", got)
	}
}
