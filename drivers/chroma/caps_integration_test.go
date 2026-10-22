//go:build integration

package chroma

import (
	"context"
	"errors"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestQueryModes(t *testing.T) {
	c := open(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 8}})
	// get with a where clause: ages above 60 (ids 41..44; 44 has none).
	_, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"tobira_people","where":{"age":{"$gt":60}}}`})
	if len(rows) != 3 {
		t.Errorf("age > 60: %d rows, want 3", len(rows))
	}
	// ids, include embeddings, limit and offset.
	cols, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"tobira_people","ids":["3","1","2"],"include":["embeddings"],"limit":2,"offset":1}`})
	if len(rows) != 2 || colIndex(t, cols, "embedding") < 0 || len(cols) != 2 {
		t.Errorf("ids/include/limit/offset: %d rows, columns %v", len(rows), cols)
	}
	if v, ok := rows[0][1].AsVector(); !ok || len(v) != 2 {
		t.Errorf("embedding = %s", rows[0][1])
	}
	// whereDocument (snake_case spelling).
	cols, rows, _ = collect(t, c, driver.Query{Text: `{"collection":"texts","where_document":{"$contains":"database"}}`})
	if len(rows) != 2 {
		t.Errorf("document contains database: %d rows", len(rows))
	}
	if colIndex(t, cols, "document") != 1 {
		t.Errorf("columns = %v", cols)
	}
	// query by embedding: Chroma's own (squared L2) distance.
	cols, rows, _ = collect(t, c, driver.Query{Text: `{"collection":"vecs","queryEmbeddings":[[1,0,0,0]],"nResults":2}`})
	if len(rows) != 2 {
		t.Fatalf("query: %d rows", len(rows))
	}
	if d, _ := rows[0][colIndex(t, cols, "_distance")].AsFloat(); math.Abs(d-0.1) > 1e-5 {
		t.Errorf("raw l2 distance = %g, want 0.1 (squared)", d)
	}
	// query by text, through the host embedder: each text finds itself.
	cols, rows, _ = collect(t, c, driver.Query{Text: `{"collection":"texts","queryTexts":["cooking pasta at home","vector search with chroma"],"nResults":1}`, ReadOnly: true})
	if len(rows) != 2 {
		t.Fatalf("queryTexts: %d rows", len(rows))
	}
	ii := colIndex(t, cols, "id")
	if a, _ := rows[0][ii].AsText(); a != "t3" {
		t.Errorf("first text found %s, want t3", rows[0][ii])
	}
	if b, _ := rows[1][ii].AsText(); b != "t4" {
		t.Errorf("second text found %s, want t4", rows[1][ii])
	}
	// count.
	_, rows, _ = collect(t, c, driver.Query{Text: `{"op":"count","collection":"tobira_people"}`})
	if len(rows) != 1 || rows[0][0].String() != "57" {
		t.Errorf("count = %v", rows)
	}
	// Errors from the server keep their kind.
	if _, err := c.Query(ctx30(t), driver.Query{Text: `{"collection":"tobira_people","where":{"age":{"$bogus":1}}}`}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad where: want ErrSyntax, got %v", err)
	}
	if _, err := c.Query(ctx30(t), driver.Query{Text: `{"collection":"vecs","queryEmbeddings":[[1,0]]}`}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("wrong query dimension: want ErrSyntax, got %v", err)
	}
}

// The collection's stored configuration names OpenAI as its embedding
// function. Query texts still go only to the host embedder, and without
// one they are refused: tobira never falls back to the collection's
// function.
func TestCollectionEmbeddingFunctionNeverUsed(t *testing.T) {
	emb := &countingEmbedder{dims: 8}
	c := open(t, driver.ConnConfig{Embedder: emb, ReadOnly: true})
	_, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"ef_openai","queryTexts":["hello"],"nResults":1}`})
	if len(rows) != 1 || emb.calls != 1 {
		t.Errorf("queryTexts on ef_openai: %d rows, %d embedder calls", len(rows), emb.calls)
	}
	rs, err := c.Similar(ctx30(t), driver.CollectionRef{Name: efColl}, driver.VectorQuery{Text: "hello", K: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	if emb.calls != 2 {
		t.Errorf("Similar text: %d embedder calls", emb.calls)
	}
	bare := open(t, driver.ConnConfig{ReadOnly: true})
	for name, err := range map[string]error{
		"query": func() error {
			_, err := bare.Query(ctx30(t), driver.Query{Text: `{"collection":"ef_openai","queryTexts":["hello"]}`})
			return err
		}(),
		"similar": func() error {
			_, err := bare.Similar(ctx30(t), driver.CollectionRef{Name: efColl}, driver.VectorQuery{Text: "hello"})
			return err
		}(),
	} {
		if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), driver.NeedEmbedderHint) {
			t.Errorf("%s without an embedder: want ErrUnsupported with the hint, got %v", name, err)
		}
	}
}

func TestReadOnlyRefusals(t *testing.T) {
	ro := open(t, driver.ConnConfig{ReadOnly: true})
	ctx, ref := ctx30(t), driver.CollectionRef{Name: peopleColl}
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	_, insErr := ro.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(5000)}, {Key: "embedding", Value: driver.Vector([]float32{1, 1})}})
	for name, err := range map[string]error{
		"insert": insErr,
		"update": ro.Update(ctx, ref, key, driver.Object{{Key: "name", Value: driver.String("x")}}),
		"delete": ro.Delete(ctx, ref, key),
	} {
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s: want ErrReadOnly, got %v", name, err)
		}
	}
	for _, text := range []string{
		`{"op":"delete","collection":"tobira_people","ids":["1"]}`,
		`{"op":"upsert","collection":"tobira_people","ids":["1"],"embeddings":[[0,0]]}`,
		`{"op":"fork","collection":"tobira_people","new_name":"copy"}`,
		`{"op":"search","collection":"tobira_people"}`,
	} {
		if _, err := ro.Query(ctx, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s: want ErrReadOnly, got %v", text, err)
		}
	}
	cols, rows, _ := collect(t, ro, driver.Query{Text: `{"collection":"tobira_people"}`})
	if len(rows) != 57 {
		t.Errorf("People after refusals: %d rows", len(rows))
	}
	ni := colIndex(t, cols, "name")
	for _, r := range rows {
		if id, _ := r[0].AsInt(); id == 1 && r[ni].String() != "Ōtani Shōhei" {
			t.Errorf("record 1 changed: %s", r[ni])
		}
	}
	// Reads still work read-only, Browse and Similar included.
	if _, err := ro.Browse(ctx, ref, driver.BrowseOptions{Limit: 5, ReadOnly: true}); err != nil {
		t.Errorf("read-only Browse: %v", err)
	}
	if _, err := ro.Similar(ctx, driver.CollectionRef{Name: vecsColl}, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}}); err != nil {
		t.Errorf("read-only Similar: %v", err)
	}
}

func browseAll(t *testing.T, c *conn, ref driver.CollectionRef, opts driver.BrowseOptions) ([]driver.Column, []driver.Row) {
	t.Helper()
	var (
		cols []driver.Column
		all  []driver.Row
	)
	for range 100 {
		rs, err := c.Browse(ctx30(t), ref, opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", opts, err)
		}
		cols = rs.Columns()
		rows, st, err := driver.Collect(ctx30(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
		if opts.Cursor = st.NextCursor; opts.Cursor == "" {
			return cols, all
		}
	}
	t.Fatal("Browse never ended")
	return nil, nil
}

func TestBrowse(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	people := driver.CollectionRef{Name: peopleColl}
	f := func(conds ...driver.Condition) driver.Filter { return driver.Filter{Conditions: conds} }
	for name, tc := range map[string]struct {
		ref    driver.CollectionRef
		filter driver.Filter
		want   int
	}{
		"age gt":            {people, f(cond("age", driver.OpGt, driver.Int(60))), 3},
		"age in":            {people, f(cond("age", driver.OpIn, driver.List(driver.Int(21), driver.Int(22)))), 4}, // ids 1, 2, 46, 47
		"active and age le": {people, f(cond("active", driver.OpEq, driver.Bool(true)), cond("age", driver.OpLe, driver.Int(30))), 0},
		"name ne":           {people, f(cond("name", driver.OpNe, driver.String("Ada Lovelace"))), 53},
		"id eq":             {people, f(cond("id", driver.OpEq, driver.Int(7))), 1},
		"id in":             {people, f(cond("id", driver.OpIn, driver.List(driver.Int(7), driver.Int(8), driver.Int(999)))), 2},
		"id eq none":        {people, f(cond("id", driver.OpEq, driver.Int(7)), cond("id", driver.OpEq, driver.Int(8))), 0},
		"score ge":          {people, f(cond("score", driver.OpGe, driver.Decimal("400"))), 0},
		"raw":               {people, driver.Filter{Raw: `{"$or":[{"age":{"$eq":21}},{"age":{"$eq":22}}]}`}, 4},
		"document contains": {driver.CollectionRef{Name: textsColl}, f(cond("document", driver.OpContains, driver.String("database"))), 2},
		"document prefix":   {driver.CollectionRef{Name: textsColl}, f(cond("document", driver.OpPrefix, driver.String("the "))), 1},
	} {
		_, rows := browseAll(t, c, tc.ref, driver.BrowseOptions{Limit: 10, Filter: tc.filter})
		if name == "active and age le" || name == "score ge" {
			// Counted from the fixture rather than by hand.
			tc.want = countPeople(name)
		}
		if len(rows) != tc.want {
			t.Errorf("%s: %d rows, want %d", name, len(rows), tc.want)
		}
	}
	for _, filter := range []driver.Filter{
		f(cond("name", driver.OpContains, driver.String("a"))),
		f(cond("age", driver.OpIsNull, driver.Null())),
	} {
		if _, err := c.Browse(ctx30(t), people, driver.BrowseOptions{Filter: filter}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Browse(%+v): want ErrUnsupported, got %v", filter, err)
		}
	}
	// Sorted paging: pages of 10 by age descending, no overlap, NULLs last.
	cols, rows := browseAll(t, c, people, driver.BrowseOptions{Limit: 10, Sort: []driver.SortKey{{Field: "age", Desc: true}}})
	if len(rows) != 57 {
		t.Fatalf("sorted pages: %d rows", len(rows))
	}
	ai, ei := colIndex(t, cols, "age"), colIndex(t, cols, "embedding")
	seen := map[string]bool{}
	prev := int64(math.MaxInt64)
	for i, r := range rows {
		if seen[r[0].String()] {
			t.Errorf("id %s twice", r[0])
		}
		seen[r[0].String()] = true
		if r[ei].IsNull() {
			t.Errorf("row %d has no embedding", i)
		}
		if i >= 52 {
			if !r[ai].IsNull() {
				t.Errorf("row %d age %s, want the NULLs last", i, r[ai])
			}
			continue
		}
		n, _ := r[ai].AsInt()
		if n > prev {
			t.Errorf("row %d age %d after %d", i, n, prev)
		}
		prev = n
	}
}

// countPeople counts fixture rows for the filters TestBrowse names.
func countPeople(name string) int {
	cols := drivertest.People.Columns
	idx := func(n string) int {
		return slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == n })
	}
	n := 0
	for _, r := range drivertest.People.Rows {
		switch name {
		case "active and age le":
			on, _ := r[idx("active")].AsBool()
			age, ok := r[idx("age")].AsInt()
			if on && ok && age <= 30 {
				n++
			}
		case "score ge":
			s, _ := r[idx("score")].AsText()
			if f, _ := strconv.ParseFloat(s, 64); f >= 400 {
				n++
			}
		}
	}
	return n
}

func TestMutatorSemantics(t *testing.T) {
	const coll = "tobira_scratch"
	if _, err := createColl(ctx30(t), defaultDB, coll, "l2", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dropColl(defaultDB, coll) })
	c := open(t, driver.ConnConfig{})
	ctx, ref := ctx30(t), driver.CollectionRef{Name: coll}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	key, err := c.Insert(ctx, ref, driver.Object{
		{Key: "id", Value: driver.Int(1)}, {Key: "embedding", Value: driver.Vector([]float32{1, 0, 0})},
		{Key: "document", Value: driver.String("first")}, {Key: "uri", Value: driver.String("s3://b/1")},
		{Key: "n", Value: driver.Int(5)}, {Key: "f", Value: driver.Float(2)}, {Key: "b", Value: driver.Bool(true)},
		{Key: "s", Value: driver.String("hé")}, {Key: "t", Value: driver.Time(at)}, {Key: "j", Value: driver.JSON([]byte(`{"a": [1]}`))},
		{Key: "l", Value: driver.List(driver.String("x"), driver.String("y"))}, {Key: "gone", Value: driver.Null()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := key.Get("id"); v.String() != "1" || v.Type() != driver.TypeInt {
		t.Errorf("key = %v", key)
	}
	read := func() map[string]driver.Value {
		t.Helper()
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"` + coll + `","ids":["1"],"include":["documents","uris","metadatas","embeddings"]}`})
		if len(rows) != 1 {
			t.Fatalf("record 1: %d rows", len(rows))
		}
		out := map[string]driver.Value{}
		for i, col := range cols {
			out[col.Name] = rows[0][i]
		}
		return out
	}
	got := read()
	for name, want := range map[string]driver.Value{
		"document": driver.String("first"), "uri": driver.String("s3://b/1"), "n": driver.Int(5), "f": driver.Float(2),
		"b": driver.Bool(true), "s": driver.String("hé"), "t": driver.String("2026-03-04T05:06:07Z"),
		"j": driver.String(`{"a":[1]}`), "l": driver.List(driver.String("x"), driver.String("y")),
		"embedding": driver.Vector([]float32{1, 0, 0}),
	} {
		if g := got[name]; g.Type() != want.Type() || !drivertest.EqualValue(want, g) {
			t.Errorf("%s = %s (%s), want %s (%s)", name, g, g.Type(), want, want.Type())
		}
	}
	if _, ok := got["gone"]; ok {
		t.Error("a NULL member was written on insert")
	}

	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"conflict": {func() error {
			_, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "embedding", Value: driver.Vector([]float32{0, 1, 0})}})
			return err
		}(), driver.ErrConflict},
		"no embedding": {func() error {
			_, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(2)}})
			return err
		}(), driver.ErrSyntax},
		"wrong dimension": {func() error {
			_, err := c.Insert(ctx, ref, driver.Object{{Key: "embedding", Value: driver.Vector([]float32{1, 0})}})
			return err
		}(), driver.ErrSyntax},
		"update missing": {c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(99)}}, driver.Object{{Key: "n", Value: driver.Int(1)}}), driver.ErrNotFound},
		"delete missing": {c.Delete(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(99)}}), driver.ErrNotFound},
		"change id":      {c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "id", Value: driver.Int(2)}}), driver.ErrUnsupported},
		"empty list":     {c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "l", Value: driver.List()}}), driver.ErrUnsupported},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, tc.err)
		}
	}
	if got := read(); !drivertest.EqualValue(driver.Vector([]float32{1, 0, 0}), got["embedding"]) {
		t.Errorf("the refused conflicting insert changed the record: %s", got["embedding"])
	}

	// Update merges; NULL removes a key; the embedding and document change.
	if err := c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{
		{Key: "n", Value: driver.Null()}, {Key: "s", Value: driver.String("new")},
		{Key: "embedding", Value: driver.Vector([]float32{0, 0, 1})}, {Key: "document", Value: driver.String("second")},
	}); err != nil {
		t.Fatal(err)
	}
	got = read()
	if _, ok := got["n"]; ok {
		t.Errorf("n still present after a NULL update: %s", got["n"])
	}
	if got["s"].String() != "new" || got["document"].String() != "second" || !drivertest.EqualValue(driver.Vector([]float32{0, 0, 1}), got["embedding"]) || got["b"].String() != "true" {
		t.Errorf("after update: %v", got)
	}

	// A generated id.
	key, err = c.Insert(ctx, ref, driver.Object{{Key: "embedding", Value: driver.Vector([]float32{1, 1, 1})}})
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := key.Get("id")
	if s, _ := gen.AsText(); !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(s) {
		t.Errorf("generated id %s", gen)
	}
	if err := c.Delete(ctx, ref, key); err != nil {
		t.Fatal(err)
	}

	// Metadata keys that clash with record columns are renamed, and
	// editing the renamed column edits the metadata key, never the record.
	id, _ := c.collection(ctx, "test", ref)
	if err := admin().add(ctx, defaultTenant, defaultDB, id.id, addRequest{IDs: []string{"clash"}, Embeddings: [][]float32{{0, 1, 1}},
		Metadatas: []map[string]any{{"id": "shadow", "document": "meta doc", "meta_x": 1}}}); err != nil {
		t.Fatal(err)
	}
	cols, rows := browseAll(t, c, ref, driver.BrowseOptions{Limit: 10, Filter: driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpEq, driver.String("clash"))}}})
	if len(rows) != 1 {
		t.Fatalf("clash: %d rows", len(rows))
	}
	for name, want := range map[string]string{"id": "clash", "meta_id": "shadow", "meta_document": "meta doc", "meta_meta_x": "1"} {
		if v := rows[0][colIndex(t, cols, name)]; v.String() != want {
			t.Errorf("%s = %s, want %s", name, v, want)
		}
	}
	if err := c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.String("clash")}}, driver.Object{{Key: "meta_id", Value: driver.String("edited")}}); err != nil {
		t.Fatal(err)
	}
	_, rows = browseAll(t, c, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{cond("meta_id", driver.OpEq, driver.String("edited"))}}})
	if len(rows) != 1 || rows[0][0].String() != "clash" {
		t.Errorf("filter on the renamed column after the edit: %v", rows)
	}
}

func TestSimilarMetrics(t *testing.T) {
	c := open(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 8}})
	ctx := ctx30(t)
	q := []float32{1, 0, 0, 0}
	for _, tc := range []struct {
		coll   string
		metric driver.VectorMetric
		top    int64
		dist   float64
	}{
		{vecsColl, driver.MetricL2, 1, math.Sqrt(0.1)}, // sqrt(0.1² + 0.3²)
		{vecsCos, driver.MetricCosine, 2, 1 - 5/math.Sqrt(25.01)},
		{vecsIP, driver.MetricInnerProduct, 3, -6},
	} {
		for _, m := range []driver.VectorMetric{driver.MetricDefault, tc.metric} {
			rs, err := c.Similar(ctx, driver.CollectionRef{Name: tc.coll}, driver.VectorQuery{Vector: q, K: 3, Metric: m})
			if err != nil {
				t.Fatalf("%s %q: %v", tc.coll, m, err)
			}
			cols := rs.Columns()
			rows, _, err := driver.Collect(ctx, rs, 0)
			if err != nil {
				t.Fatal(err)
			}
			if id, _ := rows[0][colIndex(t, cols, "id")].AsInt(); id != tc.top {
				t.Errorf("%s: top %s, want %d", tc.coll, rows[0][0], tc.top)
			}
			if d, _ := rows[0][colIndex(t, cols, "_distance")].AsFloat(); math.Abs(d-tc.dist) > 1e-4 {
				t.Errorf("%s: _distance %g, want %g", tc.coll, d, tc.dist)
			}
		}
		other := driver.MetricCosine
		if tc.metric == other {
			other = driver.MetricL2
		}
		if _, err := c.Similar(ctx, driver.CollectionRef{Name: tc.coll}, driver.VectorQuery{Vector: q, Metric: other}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s by %s: want ErrUnsupported, got %v", tc.coll, other, err)
		}
	}
	// A filter runs on the server.
	rs, err := c.Similar(ctx, driver.CollectionRef{Name: vecsColl}, driver.VectorQuery{Vector: q, K: 3, Filter: driver.Filter{Conditions: []driver.Condition{cond("n", driver.OpGt, driver.Int(30))}}})
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	rows, _, _ := driver.Collect(ctx, rs, 0)
	for _, r := range rows {
		if n, _ := r[colIndex(t, cols, "n")].AsInt(); n <= 30 {
			t.Errorf("filtered Similar returned n = %d", n)
		}
	}
	if len(rows) != 3 {
		t.Errorf("filtered Similar: %d rows", len(rows))
	}
	// Text through the host embedder finds the same text first.
	rs, err = c.Similar(ctx, driver.CollectionRef{Name: textsColl}, driver.VectorQuery{Text: textDocs[1], K: 2})
	if err != nil {
		t.Fatal(err)
	}
	cols = rs.Columns()
	rows, _, _ = driver.Collect(ctx, rs, 0)
	if len(rows) != 2 || rows[0][colIndex(t, cols, "id")].String() != "t2" {
		t.Errorf("text search: %v", rows)
	}
	if d, _ := rows[0][colIndex(t, cols, "_distance")].AsFloat(); d > 1e-3 {
		t.Errorf("text search: distance to itself %g", d)
	}
	if _, err := c.Similar(ctx, driver.CollectionRef{Name: vecsColl}, driver.VectorQuery{Text: "x"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("8-dim embedding on a 4-dim collection: want ErrSyntax, got %v", err)
	}
}

func TestCatalog(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ns := range nss {
		names = append(names, ns.Name)
		if ns.Default != (ns.Name == defaultDB) {
			t.Errorf("namespace %+v", ns)
		}
	}
	if !slices.Contains(names, defaultDB) || !slices.Contains(names, otherDB) {
		t.Errorf("namespaces = %v", names)
	}
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: otherDB})
	if err != nil || len(cols) != 1 || cols[0].Ref.Name != "elsewhere" {
		t.Errorf("collections(%s) = %v, %v", otherDB, cols, err)
	}
	if _, _, st := collect(t, c, driver.Query{Text: `{"collection":"elsewhere"}`, Namespace: otherDB}); st.Truncated {
		t.Error("truncated")
	}
	sch, err := c.Describe(ctx, driver.CollectionRef{Name: vecsCos})
	if err != nil {
		t.Fatal(err)
	}
	var native string
	for _, f := range sch.Fields {
		if f.Name == "embedding" {
			native = f.NativeType
		}
	}
	if native != "vector(4, cosine)" || sch.RowEstimate != 6 || sch.Indexes[0].Method != "hnsw cosine" || !strings.Contains(sch.DDL, "cosine") {
		t.Errorf("describe vecs_cos: native %q, rows %d, indexes %+v", native, sch.RowEstimate, sch.Indexes)
	}
	// Direct to the container, the identity fills tenant and database.
	direct := open(t, driver.ConnConfig{DSN: "chroma://" + chromaAddr})
	if direct.tenant != defaultTenant || direct.database != defaultDB {
		t.Errorf("direct: %s/%s", direct.tenant, direct.database)
	}
	if _, err := New().Open(ctx, driver.ConnConfig{DSN: "chroma://" + chromaAddr + "?database=nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing database: want ErrNotFound, got %v", err)
	}
}

func TestAuth(t *testing.T) {
	const bad = "bad-token-zz"
	for name, tc := range map[string]struct {
		cfg  driver.ConnConfig
		want error
	}{
		"password ref":      {driver.ConnConfig{DSN: testDSN(""), Password: driver.NewSecret(testToken)}, nil},
		"password position": {driver.ConnConfig{DSN: "chroma://:" + testToken + "@" + proxyAddr}, nil},
		"token parameter":   {driver.ConnConfig{DSN: testDSN("token=" + testToken)}, nil},
		"bearer":            {driver.ConnConfig{DSN: testDSN("auth_header=authorization"), Password: driver.NewSecret(testToken)}, nil},
		"no token":          {driver.ConnConfig{DSN: testDSN("")}, driver.ErrAuth},
		"wrong token":       {driver.ConnConfig{DSN: testDSN("tenant=default_tenant&database=default_database"), Password: driver.NewSecret(bad)}, driver.ErrAuth},
	} {
		c, err := New().Open(ctx30(t), tc.cfg)
		if err == nil {
			_ = c.Close()
		}
		if tc.want == nil && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, err)
		}
		if err != nil && (strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), testToken)) {
			t.Errorf("%s: the error carries the token: %v", name, err)
		}
	}
}

func TestCancellation(t *testing.T) {
	c := open(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 8}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, qErr := c.Query(ctx, driver.Query{Text: `{"collection":"tobira_people"}`})
	_, bErr := c.Browse(ctx, driver.CollectionRef{Name: peopleColl}, driver.BrowseOptions{})
	_, sErr := c.Similar(ctx, driver.CollectionRef{Name: textsColl}, driver.VectorQuery{Text: "x"})
	_, dErr := c.Describe(ctx, driver.CollectionRef{Name: peopleColl})
	for name, err := range map[string]error{"query": qErr, "browse": bErr, "similar": sErr, "describe": dErr} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: want context.Canceled, got %v", name, err)
		}
	}
	// An expired deadline.
	short, cancel2 := context.WithTimeout(context.Background(), time.Microsecond)
	defer cancel2()
	time.Sleep(time.Millisecond)
	if _, err := c.Query(short, driver.Query{Text: `{"collection":"tobira_people"}`}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expired deadline: want DeadlineExceeded, got %v", err)
	}
	if err := c.Ping(ctx30(t)); err != nil {
		t.Errorf("Ping after cancellations: %v", err)
	}
}
