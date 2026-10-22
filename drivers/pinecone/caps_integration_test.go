//go:build integration

package pinecone

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

var fixtureQuery = drivertest.KindVectorFixture(driver.CollectionRef{}).Query

// Each metric ranks the vector fixture differently, and each lives on its
// own index: the winners prove the index metric is the one searched.
func TestMetrics(t *testing.T) {
	want := map[string]driver.Value{mainIndex: driver.Int(1), cosIndex: driver.Int(2), dotIndex: driver.Int(3)}
	for name, top := range want {
		c := open(t, driver.ConnConfig{DSN: indexDSN(name)})
		ref := driver.CollectionRef{Namespace: name, Name: vectorsNS}
		q := fixtureQuery
		q.Metric = driver.MetricDefault
		cols, rows, _ := collect(t)(c.Similar(ctx30(t), ref, q))
		if len(rows) != 3 {
			t.Fatalf("%s: %d rows, want 3", name, len(rows))
		}
		id, di := colIndex(t, cols, "id"), colIndex(t, cols, distanceColumn)
		if !drivertest.EqualValue(rows[0][id], top) {
			t.Errorf("%s: nearest is %s, want %s", name, rows[0][id], top)
		}
		prev := -1e300
		for _, r := range rows {
			d, _ := r[di].AsFloat()
			if d < prev {
				t.Errorf("%s: distances not ascending: %v", name, rows)
			}
			prev = d
		}
		// Asking for the index's own metric works; any other is refused.
		q.Metric = metricOf(indexMetric[name])
		if _, err := c.Similar(ctx30(t), ref, q); err != nil {
			t.Errorf("%s with its own metric: %v", name, err)
		}
		for _, m := range []driver.VectorMetric{driver.MetricL2, driver.MetricCosine, driver.MetricInnerProduct} {
			if m == metricOf(indexMetric[name]) {
				continue
			}
			q.Metric = m
			if _, err := c.Similar(ctx30(t), ref, q); !errors.Is(err, driver.ErrUnsupported) {
				t.Errorf("%s with metric %s: %v, want ErrUnsupported", name, m, err)
			}
		}
	}
}

// pinecone-local reports no metric: without metric= in the DSN a vector
// search explains what to set, while a native query still runs.
func TestUnknownMetric(t *testing.T) {
	c := open(t, driver.ConnConfig{DSN: "pinecone://?host=" + indexAddr[mainIndex] + "&tls=false&index=" + mainIndex})
	_, err := c.Similar(ctx30(t), driver.CollectionRef{Namespace: mainIndex, Name: vectorsNS}, fixtureQuery)
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "metric=") {
		t.Errorf("Similar without a known metric: %v", err)
	}
	_, rows, _ := collect(t)(c.Query(ctx30(t), driver.Query{Text: `{"namespace":"vectors","vector":[1,0,0,0],"topK":2}`}))
	if len(rows) != 2 {
		t.Errorf("query: %d rows", len(rows))
	}
}

func TestSimilarFilterAndText(t *testing.T) {
	c := open(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 4}})
	ref := driver.CollectionRef{Namespace: mainIndex, Name: vectorsNS}
	q := fixtureQuery
	q.K = 10
	q.Filter = driver.Filter{Conditions: []driver.Condition{{Field: "label", Op: driver.OpEq, Value: driver.String("large")}}}
	cols, rows, _ := collect(t)(c.Similar(ctx30(t), ref, q))
	if len(rows) != 1 || !drivertest.EqualValue(rows[0][colIndex(t, cols, "id")], driver.Int(3)) {
		t.Errorf("label = large: %v", rows)
	}
	q.Filter = driver.Filter{Raw: `{"n":{"$gte":40}}`}
	cols, rows, _ = collect(t)(c.Similar(ctx30(t), ref, q))
	var ids []int64
	for _, r := range rows {
		n, _ := r[colIndex(t, cols, "id")].AsInt()
		ids = append(ids, n)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{4, 5, 6}) {
		t.Errorf("n >= 40: ids %v", ids)
	}
	_, rows, _ = collect(t)(c.Similar(ctx30(t), ref, driver.VectorQuery{Text: "near things", K: 2}))
	if len(rows) != 2 {
		t.Errorf("text search: %d rows", len(rows))
	}
	if _, err := c.Similar(ctx30(t), driver.CollectionRef{Namespace: mainIndex, Name: "tobira_no_such_ns"}, fixtureQuery); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing namespace: %v", err)
	}
}

func TestQueryOps(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	// query by vector, with a filter and values.
	cols, rows, _ := collect(t)(c.Query(ctx, driver.Query{Text: `{"namespace":"vectors","vector":[0.9,0.3,0,0],"topK":3,"filter":{"n":{"$lte":30}},"includeValues":true}`}))
	if len(rows) != 3 || cols[1].Name != scoreColumn || cols[2].Name != valuesColumn || cols[2].Type != driver.TypeVector {
		t.Fatalf("query: cols %v rows %v", cols, rows)
	}
	if id, _ := rows[0][0].AsText(); id != "1" {
		t.Errorf("query: nearest %s", rows[0][0])
	}
	// query by id, without metadata.
	cols, rows, _ = collect(t)(c.Query(ctx, driver.Query{Text: `{"op":"query","namespace":"vectors","id":"2","topK":1,"includeMetadata":false}`}))
	if len(rows) != 1 || len(cols) != 2 {
		t.Errorf("query by id: cols %v rows %v", cols, rows)
	}
	// fetch keeps the order of ids and skips missing ones.
	_, rows, _ = collect(t)(c.Query(ctx, driver.Query{Text: `{"op":"fetch","namespace":"tobira_people","ids":["7","nope","3"]}`}))
	if len(rows) != 2 || rows[0][0] != driver.String("7") || rows[1][0] != driver.String("3") {
		t.Errorf("fetch: %v", rows)
	}
	// list by prefix, and with a limit.
	_, rows, _ = collect(t)(c.Query(ctx, driver.Query{Text: `{"op":"list","namespace":"tobira_people","prefix":"5"}`}))
	if len(rows) != 9 {
		t.Errorf("list prefix 5: %d rows, want 9 (5, 50..57)", len(rows))
	}
	_, rows, st := collect(t)(c.Query(ctx, driver.Query{Text: `{"op":"list","namespace":"tobira_people","limit":5}`}))
	if len(rows) != 5 || st.Truncated {
		t.Errorf("list limit 5: %d rows, truncated %v", len(rows), st.Truncated)
	}
	// stats: one row per namespace.
	_, rows, _ = collect(t)(c.Query(ctx, driver.Query{Text: `{"op":"stats"}`}))
	counts := map[string]int64{}
	for _, r := range rows {
		s, _ := r[0].AsText()
		counts[s], _ = r[1].AsInt()
	}
	if counts[peopleNS] != 57 || counts[vectorsNS] != 6 {
		t.Errorf("stats: %v", counts)
	}
	// The default index comes from Query.Namespace too.
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"stats"}`, Namespace: "other"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("stats on another index: %v", err)
	}
	// A server-side refusal maps to ErrSyntax.
	if _, err := c.Query(ctx, driver.Query{Text: `{"namespace":"vectors","vector":[1,2,3]}`}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("wrong dimension: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"namespace":"vectors","vector":[1,2,3,4],"filter":{"n":{"$bogus":1}}}`}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad filter: %v", err)
	}
}

func TestDescribe(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	s, err := c.Describe(ctx30(t), driver.CollectionRef{Namespace: mainIndex, Name: peopleNS})
	if err != nil {
		t.Fatal(err)
	}
	if s.RowEstimate != 57 || !s.Schemaless || len(s.Sample) != 57 || !slices.Equal(s.PrimaryKey, []string{idColumn}) {
		t.Errorf("describe: estimate %d schemaless %v sample %d key %v", s.RowEstimate, s.Schemaless, len(s.Sample), s.PrimaryKey)
	}
	if !strings.Contains(s.Comment, "4 dimensions") || !strings.Contains(s.Comment, "euclidean") {
		t.Errorf("comment %q", s.Comment)
	}
	if s.Fields[0].Name != idColumn || s.Fields[1].Name != valuesColumn || s.Fields[1].NativeType != "vector(4)" {
		t.Errorf("fields %+v", s.Fields[:2])
	}
	cs, err := c.Collections(ctx30(t), driver.NamespaceRef{Name: mainIndex})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, col := range cs {
		if col.Ref.Name == peopleNS && col.RowEstimate == 57 {
			found = true
		}
	}
	if !found {
		t.Errorf("collections %+v", cs)
	}
}

// A project DSN lists the indexes through the control plane. pinecone-local
// reports each index host on its container port, which the test maps to a
// random one, so the data plane of a project DSN is not reachable here; the
// host resolution is covered by TestControlPlane against a fake.
func TestControlPlaneMode(t *testing.T) {
	c := open(t, driver.ConnConfig{DSN: "pinecone://" + controlAddr + "?tls=false"})
	nss, err := c.Namespaces(ctx30(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ns := range nss {
		names = append(names, ns.Name)
	}
	if !slices.Equal(names, []string{mainIndex, cosIndex, dotIndex}) {
		t.Errorf("namespaces %v", names)
	}
	if _, err := c.Query(ctx30(t), driver.Query{Text: `{"op":"stats","index":"tobira-missing"}`}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing index: %v", err)
	}
}

func TestBrowseScan(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ref := driver.CollectionRef{Namespace: mainIndex, Name: peopleNS}
	ctx := ctx30(t)
	// A filtered browse pages through every match, stopping mid list page.
	active := driver.Filter{Conditions: []driver.Condition{{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)}}}
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		cols, rows, st := collect(t)(c.Browse(ctx, ref, driver.BrowseOptions{Limit: 7, Filter: active, Cursor: cursor}))
		pages++
		ai := colIndex(t, cols, "active")
		for _, r := range rows {
			if r[ai] != driver.Bool(true) {
				t.Errorf("inactive row %v", r)
			}
			id, _ := r[0].AsText()
			if seen[id] {
				t.Errorf("record %s twice", id)
			}
			seen[id] = true
		}
		if cursor = st.NextCursor; cursor == "" || pages > 20 {
			break
		}
	}
	if len(seen) != 38 {
		t.Errorf("active records: %d, want 38", len(seen))
	}
	// A sorted browse pages consistently; _id sorts as text.
	var order []string
	cursor = ""
	for range 10 {
		_, rows, st := collect(t)(c.Browse(ctx, ref, driver.BrowseOptions{Limit: 20, Sort: []driver.SortKey{{Field: idColumn, Desc: true}}, Cursor: cursor}))
		for _, r := range rows {
			s, _ := r[0].AsText()
			order = append(order, s)
		}
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if len(order) != 57 || order[0] != "9" || order[56] != "1" || !slices.IsSortedFunc(order, func(a, b string) int { return strings.Compare(b, a) }) {
		t.Errorf("sorted by _id desc: %v", order)
	}
	// A prefix condition on _id.
	_, rows, _ := collect(t)(c.Browse(ctx, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: idColumn, Op: driver.OpPrefix, Value: driver.String("4")}}}}))
	if len(rows) != 11 {
		t.Errorf("_id prefix 4: %d rows, want 11 (4, 40..49)", len(rows))
	}
	if _, err := c.Browse(ctx, driver.CollectionRef{Namespace: mainIndex, Name: "tobira_no_such_ns"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing namespace: %v", err)
	}
}

func TestMutatorSemantics(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	ref := driver.CollectionRef{Namespace: mainIndex, Name: "scratch_mutator"}
	t.Cleanup(func() {
		ic, done, err := adminIndex(mainIndex, ref.Name)
		if err == nil {
			_ = ic.DeleteAllVectorsInNamespace(context.Background())
			done()
		}
	})
	vec := driver.Vector([]float32{0, 1, 0, 0})
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	key, err := c.Insert(ctx, ref, driver.Object{
		{Key: idColumn, Value: driver.String("a")}, {Key: valuesColumn, Value: vec},
		{Key: "sku", Value: driver.String("S1")}, {Key: "price", Value: driver.Decimal("9.99")},
		{Key: "at", Value: driver.Time(when)}, {Key: "tags", Value: driver.JSON([]byte(`["x","y"]`))},
		{Key: "gone", Value: driver.Null()},
	})
	if err != nil || len(key) != 1 || key[0].Value != driver.String("a") {
		t.Fatalf("Insert: %v %v", key, err)
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: idColumn, Value: driver.String("a")}, {Key: valuesColumn, Value: vec}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Insert of an existing id: %v", err)
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: idColumn, Value: driver.String("b")}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Insert without values: %v", err)
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: valuesColumn, Value: driver.Vector([]float32{1, 2})}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Insert of a wrong dimension: %v", err)
	}
	gen, err := c.Insert(ctx, ref, driver.Object{{Key: valuesColumn, Value: vec}, {Key: "sku", Value: driver.String("S2")}})
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := gen[0].Value.AsText(); len(id) != 36 {
		t.Errorf("generated id %q", id)
	}
	fetchA := func() map[string]driver.Value {
		t.Helper()
		cols, rows, _ := collect(t)(c.Query(ctx, driver.Query{Text: `{"op":"fetch","namespace":"scratch_mutator","ids":["a"],"includeValues":true}`}))
		if len(rows) != 1 {
			t.Fatalf("fetch a: %v", rows)
		}
		out := map[string]driver.Value{}
		for i, col := range cols {
			out[col.Name] = rows[0][i]
		}
		return out
	}
	got := fetchA()
	if got["price"] != driver.Float(9.99) || got["at"] != driver.String("2026-03-04T05:06:07Z") ||
		!drivertest.EqualValue(got["tags"], driver.List(driver.String("x"), driver.String("y"))) {
		t.Errorf("stored values %v", got)
	}
	if _, ok := got["gone"]; ok {
		t.Error("a NULL member was stored")
	}
	// Update by _id: metadata and the vector.
	if err := c.Update(ctx, ref, key, driver.Object{{Key: "sku", Value: driver.String("S1b")}, {Key: valuesColumn, Value: driver.List(driver.Int(0), driver.Int(0), driver.Int(1), driver.Int(0))}}); err != nil {
		t.Fatal(err)
	}
	got = fetchA()
	if got["sku"] != driver.String("S1b") || !drivertest.EqualValue(got[valuesColumn], driver.Vector([]float32{0, 0, 1, 0})) || got["price"] != driver.Float(9.99) {
		t.Errorf("after Update: %v", got)
	}
	// Update by metadata equality.
	if err := c.Update(ctx, ref, driver.Object{{Key: "sku", Value: driver.String("S1b")}}, driver.Object{{Key: "price", Value: driver.Int(5)}}); err != nil {
		t.Fatal(err)
	}
	if got = fetchA(); got["price"] != driver.Int(5) {
		t.Errorf("after Update by sku: %v", got["price"])
	}
	missing := driver.Object{{Key: idColumn, Value: driver.String("nope")}}
	if err := c.Update(ctx, ref, missing, driver.Object{{Key: "sku", Value: driver.String("x")}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Update missing: %v", err)
	}
	if err := c.Update(ctx, ref, driver.Object{{Key: "sku", Value: driver.String("none")}}, driver.Object{{Key: "x", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Update by an unmatched key: %v", err)
	}
	// Two records share a value: the key is ambiguous.
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: idColumn, Value: driver.String("c")}, {Key: valuesColumn, Value: vec}, {Key: "sku", Value: driver.String("S2")}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: "sku", Value: driver.String("S2")}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("Delete by an ambiguous key: %v", err)
	}
	if err := c.Delete(ctx, ref, key); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, ref, key); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Delete twice: %v", err)
	}
	// An Int id is its decimal text.
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: idColumn, Value: driver.Int(42)}, {Key: valuesColumn, Value: vec}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: idColumn, Value: driver.String("42")}}); err != nil {
		t.Errorf("Delete 42: %v", err)
	}
}

// A cancelled call returns promptly with the context error and leaves the
// conn usable.
func TestCancellation(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ref := driver.CollectionRef{Namespace: mainIndex, Name: peopleNS}
	if _, err := c.Browse(ctx, ref, driver.BrowseOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Browse: %v", err)
	}
	if _, err := c.Similar(ctx, driver.CollectionRef{Namespace: mainIndex, Name: vectorsNS}, fixtureQuery); !errors.Is(err, context.Canceled) {
		t.Errorf("Similar: %v", err)
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: idColumn, Value: driver.String("1")}}); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete: %v", err)
	}
	// Cancel during the call itself.
	dctx, dcancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer dcancel()
	time.Sleep(2 * time.Millisecond)
	if _, err := c.stats(dctx, "query", mustIndex(t, c)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stats past the deadline: %v", err)
	}
	if err := c.Ping(ctx30(t)); err != nil {
		t.Errorf("Ping after cancellations: %v", err)
	}
}

func mustIndex(t *testing.T, c *conn) *indexHandle {
	t.Helper()
	h, err := c.index(ctx30(t), "test", mainIndex)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Metadata keys named like the record columns show with a meta_ prefix, and
// editing meta_values changes the metadata, not the vector.
func TestCollidingMetadataKeys(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	ref := driver.CollectionRef{Namespace: mainIndex, Name: "scratch_collide"}
	ic, done, err := adminIndex(mainIndex, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ic.DeleteAllVectorsInNamespace(context.Background()); done() })
	meta, _ := structpb.NewStruct(map[string]any{"values": "label", "_id": "other", "meta_x": 1})
	vals := []float32{0, 0, 0, 1}
	if _, err := ic.UpsertVectors(ctx, []*pinecone.Vector{{Id: "r1", Values: &vals, Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	cols, rows, st := collect(t)(c.Browse(ctx, ref, driver.BrowseOptions{}))
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	if rows[0][colIndex(t, cols, "meta_values")] != driver.String("label") || rows[0][colIndex(t, cols, "meta__id")] != driver.String("other") ||
		rows[0][colIndex(t, cols, "meta_meta_x")] != driver.Int(1) || rows[0][colIndex(t, cols, idColumn)] != driver.String("r1") {
		t.Errorf("row %v cols %v", rows[0], cols)
	}
	if !strings.Contains(strings.Join(st.Warnings, " "), "meta_values") {
		t.Errorf("warnings %q", st.Warnings)
	}
	key := driver.Object{{Key: idColumn, Value: driver.String("r1")}}
	if err := c.Update(ctx, ref, key, driver.Object{{Key: "meta_values", Value: driver.String("renamed")}}); err != nil {
		t.Fatal(err)
	}
	cols, rows, _ = collect(t)(c.Browse(ctx, ref, driver.BrowseOptions{}))
	if rows[0][colIndex(t, cols, "meta_values")] != driver.String("renamed") ||
		!drivertest.EqualValue(rows[0][colIndex(t, cols, valuesColumn)], driver.Vector(vals)) {
		t.Errorf("after editing meta_values: %v", rows[0])
	}
	// Filtering on the renamed column reaches the key.
	_, rows, _ = collect(t)(c.Browse(ctx, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "meta_values", Op: driver.OpEq, Value: driver.String("renamed")}}}}))
	if len(rows) != 1 {
		t.Errorf("filter on meta_values: %d rows", len(rows))
	}
}
