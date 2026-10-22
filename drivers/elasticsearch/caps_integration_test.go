//go:build integration

package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func peopleRef() driver.CollectionRef {
	return driver.CollectionRef{Namespace: defaultNamespace, Name: peopleIndex}
}

func queryRows(t *testing.T, c *conn, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("Query(%s): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatalf("Query(%s): %v", q.Text, err)
	}
	return cols, rows, st
}

func colIndex(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no column %q in %v", name, colNames(cols))
	}
	return i
}

// The read endpoints of the allow-list answer on a read-only connection.
func TestRawReadsOnReadOnlyConn(t *testing.T) {
	c := openTest(t, true)
	cols, rows, _ := queryRows(t, c, driver.Query{Text: `{"method":"POST","path":"/tobira_people/_count","body":{"query":{"term":{"active":true}}}}`})
	if v := rows[0][colIndex(t, cols, "count")]; !drivertest.EqualValue(driver.Int(38), v) {
		t.Errorf("_count = %s, want 38", v)
	}
	cols, rows, _ = queryRows(t, c, driver.Query{Text: `{"method":"GET","path":"/_cat/indices/tobira_*?format=json&h=index,docs.count"}`})
	if len(rows) != 1 || !drivertest.EqualValue(driver.String(peopleIndex), rows[0][colIndex(t, cols, "index")]) {
		t.Errorf("_cat/indices rows = %v", rows)
	}
	_, rows, _ = queryRows(t, c, driver.Query{Text: `{"method":"GET","path":"/_cat/indices/tobira_*?h=index"}`})
	if len(rows) != 1 || !drivertest.EqualValue(driver.String(peopleIndex), rows[0][0]) {
		t.Errorf("plain-text _cat lines = %v", rows)
	}
	cols, rows, _ = queryRows(t, c, driver.Query{Text: `{"method":"POST","path":"/tobira_people/_search","body":{"query":{"term":{"id":7}}}}`})
	if len(rows) != 1 || !drivertest.EqualValue(driver.String("7"), rows[0][colIndex(t, cols, "_id")]) {
		t.Errorf("raw _search rows = %v", rows)
	}
	_, rows, _ = queryRows(t, c, driver.Query{Text: `{"method":"POST","path":"/_msearch","body":[{"index":"tobira_people"},{"query":{"term":{"id":1}}},{"index":"articles"},{"query":{"match_all":{}}}]}`})
	if len(rows) != 1 {
		t.Errorf("_msearch rows = %v", rows)
	}
	for _, text := range []string{
		`{"method":"GET","path":"/tobira_people/_mapping"}`,
		`{"method":"GET","path":"/tobira_people/_doc/3"}`,
		`{"method":"GET","path":"/tobira_people/_field_caps?fields=name"}`,
		`{"method":"POST","path":"/tobira_people/_validate/query?explain=true","body":{"query":{"match_all":{}}}}`,
		`{"method":"GET","path":"/_cluster/health"}`,
		`{"method":"GET","path":"/"}`,
	} {
		if _, rows, _ := queryRows(t, c, driver.Query{Text: text}); len(rows) != 1 {
			t.Errorf("Query(%s): %d rows, want 1", text, len(rows))
		}
	}
}

// A result larger than one page streams through a point in time.
func TestQueryStreamsThroughPIT(t *testing.T) {
	c := openTest(t, false)
	c.pageSize = 10
	_, rows, st := queryRows(t, c, driver.Query{Text: `{"index":"tobira_people","sort":[{"id":"desc"}]}`})
	if len(rows) != 57 || st.Truncated {
		t.Fatalf("got %d rows (truncated %v), want 57", len(rows), st.Truncated)
	}
	_, rows, st = queryRows(t, c, driver.Query{Text: `{"index":"tobira_people","query":{"term":{"active":true}}}`, Limit: 25})
	if len(rows) != 25 || !st.Truncated {
		t.Fatalf("limit 25: got %d rows (truncated %v)", len(rows), st.Truncated)
	}
	cols, rows, _ := queryRows(t, c, driver.Query{Text: `{"index":"tobira_*","size":3,"sort":[{"id":"asc"}]}`})
	if len(rows) != 3 || colIndex(t, cols, "_index") < 0 {
		t.Errorf("a pattern search: %d rows, columns %v", len(rows), colNames(cols))
	}
}

func TestQueryAggregations(t *testing.T) {
	c := openTest(t, true)
	cols, rows, _ := queryRows(t, c, driver.Query{Text: `{"index":"tobira_people","size":0,"aggs":{"by_active":{"terms":{"field":"active"}}}}`})
	if len(rows) != 1 || len(cols) != 1 || cols[0].Name != "by_active" {
		t.Fatalf("aggregations: columns %v rows %v", colNames(cols), rows)
	}
	obj, ok := rows[0][0].AsObject()
	if !ok {
		t.Fatalf("by_active = %s, want an object", rows[0][0])
	}
	if _, ok := obj.Get("buckets"); !ok {
		t.Errorf("by_active has no buckets: %s", rows[0][0])
	}
}

func TestCollectionsHidesHiddenAndListsAliases(t *testing.T) {
	mustAdmin(t, http.MethodPut, "/tobira_hidden", map[string]any{"settings": map[string]any{"index.hidden": true}})
	mustAdmin(t, http.MethodPut, "/.tobira_dot", nil)
	mustAdmin(t, http.MethodPost, "/_aliases", map[string]any{"actions": []any{map[string]any{"add": map[string]any{"index": peopleIndex, "alias": "tobira_alias"}}}})
	t.Cleanup(func() {
		_, _ = admin(context.Background(), http.MethodDelete, "/tobira_hidden", nil)
		_, _ = admin(context.Background(), http.MethodDelete, "/.tobira_dot?expand_wildcards=all", nil)
		_, _ = admin(context.Background(), http.MethodDelete, "/"+peopleIndex+"/_alias/tobira_alias", nil)
	})
	c := openTest(t, true)
	cols, err := c.Collections(context.Background(), driver.NamespaceRef{Name: defaultNamespace})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]driver.Collection{}
	for _, col := range cols {
		byName[col.Ref.Name] = col
	}
	for _, hidden := range []string{"tobira_hidden", ".tobira_dot"} {
		if _, ok := byName[hidden]; ok {
			t.Errorf("Collections lists %s", hidden)
		}
	}
	if p := byName[peopleIndex]; p.Type != driver.CollectionIndex || p.RowEstimate != 57 {
		t.Errorf("People = %+v", p)
	}
	if a, ok := byName["tobira_alias"]; !ok || a.Type != driver.CollectionView || !strings.Contains(a.Comment, peopleIndex) {
		t.Errorf("alias = %+v (listed %v)", a, ok)
	}
	sch, err := c.Describe(context.Background(), driver.CollectionRef{Namespace: defaultNamespace, Name: "tobira_alias"})
	if err != nil {
		t.Fatal(err)
	}
	if sch.Type != driver.CollectionView || sch.RowEstimate != 57 || !slices.ContainsFunc(sch.Fields, func(f driver.Field) bool { return f.Name == "score" && f.Type == driver.TypeDecimal }) {
		t.Errorf("Describe(alias) = %+v", sch)
	}
	if _, err := c.Collections(context.Background(), driver.NamespaceRef{Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections(nope): want ErrNotFound, got %v", err)
	}
}

func browseAll(t *testing.T, c *conn, opts driver.BrowseOptions) ([]driver.Column, []driver.Row) {
	t.Helper()
	var all []driver.Row
	var cols []driver.Column
	for page := 0; page < 20; page++ {
		rs, err := c.Browse(context.Background(), peopleRef(), opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", opts, err)
		}
		cols = rs.Columns()
		rows, st, err := driver.Collect(context.Background(), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
		if st.NextCursor == "" {
			return cols, all
		}
		opts.Cursor = st.NextCursor
	}
	t.Fatal("Browse did not end")
	return nil, nil
}

func TestBrowseFiltersAndSorts(t *testing.T) {
	c := openTest(t, false)
	cond := func(f string, op driver.Op, v driver.Value) driver.BrowseOptions {
		return driver.BrowseOptions{Limit: 7, Filter: driver.Filter{Conditions: []driver.Condition{{Field: f, Op: op, Value: v}}}}
	}
	for name, tc := range map[string]struct {
		opts driver.BrowseOptions
		want int
	}{
		"prefix keyword":   {cond("email", driver.OpPrefix, driver.String("user0")), 9},
		"contains keyword": {cond("email", driver.OpContains, driver.String("r5")), 8},
		"in":               {cond("id", driver.OpIn, driver.List(driver.Int(1), driver.Int(2), driver.Int(99))), 2},
		"is null":          {cond("age", driver.OpIsNull, driver.Null()), 5},
		"ne":               {cond("active", driver.OpNe, driver.Bool(true)), 19},
		"range":            {cond("id", driver.OpGt, driver.Int(50)), 7},
		"text eq":          {cond("name", driver.OpEq, driver.String("Ada Lovelace")), 4},
		"text contains":    {cond("name", driver.OpContains, driver.String("lovelace")), 4},
		"date range":       {cond("created_at", driver.OpGe, driver.String("2026-02-01")), 29},
		"raw":              {driver.BrowseOptions{Limit: 7, Filter: driver.Filter{Raw: `{"match":{"name":"turing"}}`}}, 4},
	} {
		t.Run(name, func(t *testing.T) {
			_, rows := browseAll(t, c, tc.opts)
			if len(rows) != tc.want {
				t.Errorf("got %d rows, want %d", len(rows), tc.want)
			}
		})
	}
	cols, rows := browseAll(t, c, driver.BrowseOptions{Limit: 10, Sort: []driver.SortKey{{Field: "name"}, {Field: "id", Desc: true}}})
	ni, ii := colIndex(t, cols, "name"), colIndex(t, cols, "id")
	for i := 1; i < len(rows); i++ {
		a, _ := rows[i-1][ni].AsText()
		b, _ := rows[i][ni].AsText()
		if a > b {
			t.Fatalf("row %d name %q after %q", i, b, a)
		}
	}
	if len(rows) != 57 || rows[0][ii].String() == "" {
		t.Errorf("sorted browse: %d rows", len(rows))
	}
	if cols[0].Name != "_id" || !cols[0].Key || cols[0].Source == nil {
		t.Errorf("_id column = %+v", cols[0])
	}
	if _, err := c.Browse(context.Background(), peopleRef(), driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: `{"script":{"script":"1"}}`}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("a read-only raw script filter: want ErrReadOnly, got %v", err)
	}
	if _, err := c.Browse(context.Background(), peopleRef(), driver.BrowseOptions{Sort: []driver.SortKey{{Field: "tags"}}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("sort on a disabled object: want ErrUnsupported, got %v", err)
	}
	if _, err := c.Browse(context.Background(), driver.CollectionRef{Namespace: defaultNamespace, Name: "tobira_nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Browse(missing): want ErrNotFound, got %v", err)
	}
}

// Mutations by _id, with conflicts and the optimistic check of a key
// resolved by search.
func TestMutatorByIDAndConflicts(t *testing.T) {
	ctx := context.Background()
	mustAdmin(t, http.MethodPut, "/tobira_edit", map[string]any{"mappings": map[string]any{"properties": map[string]any{
		"name": map[string]any{"type": "keyword"}, "n": map[string]any{"type": "integer"},
	}}})
	t.Cleanup(func() { _, _ = admin(context.Background(), http.MethodDelete, "/tobira_edit", nil) })
	c := openTest(t, false)
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: "tobira_edit"}
	obj := func(kv ...any) driver.Object {
		var o driver.Object
		for i := 0; i < len(kv); i += 2 {
			o = append(o, driver.Member{Key: kv[i].(string), Value: kv[i+1].(driver.Value)})
		}
		return o
	}
	key, err := c.Insert(ctx, ref, obj("_id", driver.String("x1"), "name", driver.String("a"), "n", driver.Int(1), "_score", driver.Float(1)))
	if err != nil || !drivertest.EqualValue(driver.String("x1"), key[0].Value) {
		t.Fatalf("Insert(x1) = %v, %v", key, err)
	}
	if _, err := c.Insert(ctx, ref, obj("_id", driver.String("x1"), "name", driver.String("b"))); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Insert of an existing _id: want ErrConflict, got %v", err)
	}
	gen, err := c.Insert(ctx, ref, obj("name", driver.String("a"), "n", driver.Int(2)))
	if err != nil || gen[0].Key != "_id" {
		t.Fatalf("Insert without _id = %v, %v", gen, err)
	}
	if err := c.Update(ctx, ref, obj("name", driver.String("a")), obj("n", driver.Int(5))); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Update by a key matching two documents: want ErrConflict, got %v", err)
	}
	if err := c.Update(ctx, ref, obj("_id", driver.String("x1")), obj("n", driver.Int(10))); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, ref, obj("n", driver.Int(10)), obj("name", driver.String("renamed"))); err != nil {
		t.Fatalf("Update by a unique field: %v", err)
	}
	_, rows, _ := queryRows(t, c, driver.Query{Text: `{"index":"tobira_edit","query":{"ids":{"values":["x1"]}}}`})
	if len(rows) != 1 || !drivertest.EqualValue(driver.String("renamed"), rows[0][2]) && !drivertest.EqualValue(driver.String("renamed"), rows[0][3]) {
		t.Errorf("after updates: %v", rows)
	}
	if err := c.Update(ctx, ref, obj("_id", driver.String("x1")), obj("_id", driver.String("y"))); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("changing _id: want ErrUnsupported, got %v", err)
	}
	if err := c.Delete(ctx, ref, obj("_id", driver.String("x1"))); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, ref, obj("_id", driver.String("x1"))); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Delete of a deleted _id: want ErrNotFound, got %v", err)
	}
	if err := c.Update(ctx, ref, obj("_id", driver.String("x1")), obj("n", driver.Int(1))); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Update of a deleted _id: want ErrNotFound, got %v", err)
	}
}

func TestExplainVariants(t *testing.T) {
	c := openTest(t, true)
	ctx := context.Background()
	plan, err := c.Explain(ctx, driver.Query{Text: `{"index":"tobira_people","query":{"match":{"name":"ada"}}}`}, driver.ExplainOptions{})
	if err != nil || !strings.Contains(plan.Text, "name:ada") {
		t.Errorf("Explain = %+v, %v", plan, err)
	}
	plan, err = c.Explain(ctx, driver.Query{Text: `{"index":"tobira_people","query":{"match":{"name":"ada"}}}`, ReadOnly: true}, driver.ExplainOptions{Analyze: true})
	if err != nil || plan.Format != "json" || !strings.Contains(plan.Text, "shards") {
		t.Errorf("Explain(Analyze) = %+v, %v", plan, err)
	}
	plan, err = c.Explain(ctx, driver.Query{Text: `{"method":"POST","path":"/tobira_people/_count","body":{"query":{"term":{"id":1}}}}`}, driver.ExplainOptions{})
	if err != nil || !strings.Contains(plan.Text, "id:") {
		t.Errorf("Explain(raw _count) = %+v, %v", plan, err)
	}
	if _, err := c.Explain(ctx, driver.Query{Text: `{"index":"tobira_people","query":{"nope":{}}}`}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Explain(invalid query): want ErrSyntax, got %v", err)
	}
	w := openTest(t, false)
	if _, err := w.Explain(ctx, driver.Query{Text: peopleWrite}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Explain(a write on a writable conn): want ErrUnsupported, got %v", err)
	}
}

// The search response keeps decimals and times exact.
func TestTypedValues(t *testing.T) {
	c := openTest(t, true)
	cols, rows, _ := queryRows(t, c, driver.Query{Text: `{"index":"tobira_people","query":{"term":{"id":1}}}`})
	want := drivertest.People.Rows[0]
	for i, pc := range drivertest.People.Columns {
		got := rows[0][colIndex(t, cols, pc.Name)]
		if got.Type() != want[i].Type() || !drivertest.EqualValue(want[i], got) {
			t.Errorf("%s = %s (%s), want %s (%s)", pc.Name, got, got.Type(), want[i], want[i].Type())
		}
	}
	if s, _ := rows[0][colIndex(t, cols, "score")].AsText(); s != "12.50" {
		t.Errorf("score text = %q, want the exact source digits 12.50", s)
	}
}

// Fail-closed refusals send nothing: People is unchanged and the guard
// answers before the server would (a refused write never reaches it).
func TestReadOnlyConnRefusesMutator(t *testing.T) {
	c := openTest(t, true)
	ctx := context.Background()
	if _, err := c.Insert(ctx, peopleRef(), driver.Object{{Key: "id", Value: driver.Int(5000)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Insert: want ErrReadOnly, got %v", err)
	}
	var count struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(mustAdmin(t, http.MethodGet, "/tobira_people/_count", nil), &count)
	if count.Count != 57 {
		t.Errorf("People holds %d documents, want 57", count.Count)
	}
}
