//go:build integration

package milvus

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

var people = driver.CollectionRef{Namespace: defaultDB, Name: peopleColl}

// scratch creates a collection (id Int64 key, name VarChar, vec
// FloatVector(2) indexed L2, dynamic field on) and drops it when the test
// ends.
func scratch(t *testing.T, name string, load bool) *adminClient {
	t.Helper()
	c := adminT(t)
	ctx := ctx30(t)
	sch := entity.NewSchema().WithName(name).WithDynamicFieldEnabled(true).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("name").WithDataType(entity.FieldTypeVarChar).WithMaxLength(128).WithNullable(true)).
		WithField(entity.NewField().WithName("vec").WithDataType(entity.FieldTypeFloatVector).WithDim(2).WithNullable(true))
	if err := createIndexed(ctx, c, sch, map[string]entity.MetricType{"vec": entity.L2}, load); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.dropCollection(context.Background(), name) })
	return c
}

func loadState(t *testing.T, c *adminClient, name string) commonpb.LoadState {
	t.Helper()
	st, err := c.loadState(ctx30(t), name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A read-only connection explains an unloaded collection and never loads
// it; a writable one loads it on first use.
func TestLoadPolicy(t *testing.T) {
	c := scratch(t, "tobira_unloaded", false)
	ref := driver.CollectionRef{Name: "tobira_unloaded"}
	ro := open(t, driver.ConnConfig{ReadOnly: true})
	_, err := ro.Query(ctx30(t), driver.Query{Text: `{"collection":"tobira_unloaded"}`})
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("read-only Query on an unloaded collection = %v", err)
	}
	if _, err := ro.Browse(ctx30(t), ref, driver.BrowseOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("read-only Browse on an unloaded collection = %v", err)
	}
	if _, err := ro.Similar(ctx30(t), ref, driver.VectorQuery{Vector: []float32{1, 0}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("read-only Similar on an unloaded collection = %v", err)
	}
	rw := open(t, driver.ConnConfig{})
	if _, err := rw.Query(ctx30(t), driver.Query{Text: `{"collection":"tobira_unloaded"}`, ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Query.ReadOnly on an unloaded collection = %v", err)
	}
	if st := loadState(t, c, "tobira_unloaded"); st == commonpb.LoadState_LoadStateLoaded {
		t.Fatal("a read-only request loaded the collection")
	}
	cols, _ := ro.Collections(ctx30(t), driver.NamespaceRef{Name: defaultDB})
	i := slices.IndexFunc(cols, func(c driver.Collection) bool { return c.Ref.Name == "tobira_unloaded" })
	if i < 0 || cols[i].Comment != "not loaded" {
		t.Errorf("Collections does not flag the unloaded collection: %+v", cols)
	}
	sch, err := ro.Describe(ctx30(t), ref)
	if err != nil || !strings.Contains(sch.Comment, "not loaded") {
		t.Errorf("Describe of an unloaded collection = %+v, %v", sch, err)
	}
	_, rows, _ := collect(t, rw, driver.Query{Text: `{"collection":"tobira_unloaded"}`})
	if len(rows) != 0 {
		t.Errorf("rows %v", rows)
	}
	if st := loadState(t, c, "tobira_unloaded"); st != commonpb.LoadState_LoadStateLoaded {
		t.Errorf("a writable query did not load the collection: %v", st)
	}
}

// Browse values never become expression text: quotes, operators and
// LIKE wildcards in a value match literally.
func TestFilterValuesAreData(t *testing.T) {
	scratch(t, "tobira_inject", true)
	rw := open(t, driver.ConnConfig{})
	ref := driver.CollectionRef{Name: "tobira_inject"}
	names := []string{`x" or id > 0 or name == "`, `it's`, `50%_off`, `50xoff`, `a\b`, `plain`}
	for i, n := range names {
		if _, err := rw.Insert(ctx30(t), ref, driver.Object{{Key: "id", Value: driver.Int(int64(i + 1))}, {Key: "name", Value: driver.String(n)}}); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(opts driver.BrowseOptions) []int64 {
		t.Helper()
		cols, rows, _ := browse(t, rw, ref, opts)
		id := colIndex(t, cols, "id")
		var out []int64
		for _, r := range rows {
			n, _ := r[id].AsInt()
			out = append(out, n)
		}
		return out
	}
	cond := func(op driver.Op, v string) driver.BrowseOptions {
		return driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "name", Op: op, Value: driver.String(v)}}}}
	}
	for _, tc := range []struct {
		opts driver.BrowseOptions
		want []int64
	}{
		{cond(driver.OpEq, names[0]), []int64{1}},
		{cond(driver.OpEq, `" or id > 0 or "`), nil},
		{cond(driver.OpEq, `it's`), []int64{2}},
		{cond(driver.OpPrefix, `50`), []int64{3, 4}},
		{cond(driver.OpContains, `_`), []int64{3}},
		{cond(driver.OpContains, `_of`), []int64{3}}, // an unescaped _ would match 50xoff too
		{cond(driver.OpContains, `" or `), []int64{1}},
		{cond(driver.OpNe, `plain`), []int64{1, 2, 3, 4, 5}},
		{driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "name", Op: driver.OpIn, Value: driver.List(driver.String(`it's`), driver.String(`a\b`))}}}}, []int64{2, 5}},
	} {
		if got := ids(tc.opts); !slices.Equal(got, tc.want) {
			t.Errorf("Browse(%+v) ids = %v, want %v", tc.opts.Filter.Conditions, got, tc.want)
		}
	}
	// LIKE cannot match "%" or a backslash exactly: refused, not guessed.
	for _, v := range []string{`%`, `\`, `50%_`} {
		if _, err := rw.Browse(ctx30(t), ref, cond(driver.OpContains, v)); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("contains %q = %v, want ErrUnsupported", v, err)
		}
	}
}

// The search op and Similar: filters, params, rerank and the BM25 text
// path, which runs in Milvus and is allowed read-only.
func TestSearchAndBM25(t *testing.T) {
	c := adminT(t)
	ctx := ctx30(t)
	const name = "tobira_bm25"
	sch := entity.NewSchema().WithName(name).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("body").WithDataType(entity.FieldTypeVarChar).WithMaxLength(512).WithEnableAnalyzer(true)).
		WithField(entity.NewField().WithName("sparse").WithDataType(entity.FieldTypeSparseVector)).
		WithFunction(entity.NewFunction().WithName("bm25").WithType(entity.FunctionTypeBM25).WithInputFields("body").WithOutputFields("sparse"))
	if err := c.createCollection(ctx, sch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.dropCollection(context.Background(), name) })
	if err := c.createIndex(ctx, name, "sparse", index.NewSparseInvertedIndex(entity.BM25, 0.2)); err != nil {
		t.Fatal(err)
	}
	if err := c.insert(ctx, name,
		column.NewColumnInt64("id", []int64{1, 2, 3}),
		column.NewColumnVarChar("body", []string{"vector database search engine", "cooking with garlic", "the database of databases database"})); err != nil {
		t.Fatal(err)
	}
	rw := open(t, driver.ConnConfig{})
	// Loads it.
	collect(t, rw, driver.Query{Text: `{"op":"count","collection":"tobira_bm25"}`})

	ro := open(t, driver.ConnConfig{ReadOnly: true})
	rs, err := ro.Similar(ctx, driver.CollectionRef{Name: name}, driver.VectorQuery{Text: "database", K: 2})
	if err != nil {
		t.Fatalf("BM25 Similar (ro): %v", err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("BM25 rows %v %v", rows, err)
	}
	cols := rs.Columns()
	if id, _ := rows[0][colIndex(t, cols, "id")].AsInt(); id != 3 {
		t.Errorf("BM25 top hit = %d, want 3", id)
	}
	cols, rows, _ = collect(t, ro, driver.Query{Text: `{"collection":"tobira_bm25","text":"garlic","limit":1}`})
	if len(rows) != 1 {
		t.Fatalf("BM25 search op rows %v", rows)
	}
	if id, _ := rows[0][colIndex(t, cols, "id")].AsInt(); id != 2 {
		t.Errorf("BM25 search op top = %d, want 2", id)
	}
	if _, ok := rows[0][colIndex(t, cols, scoreColumn)].AsFloat(); !ok {
		t.Error("the search op has no numeric _score")
	}

	// Vector search op on the fixture, with a templated filter.
	cols, rows, _ = collect(t, ro, driver.Query{Text: `{"collection":"vecs","annsField":"vec_l2","vector":[1,0,0,0],"limit":3,"filter":"n >= {min}","params":{"min":20},"outputFields":["label"]}`})
	if len(rows) != 3 {
		t.Fatalf("search op rows %v", rows)
	}
	if id, _ := rows[0][colIndex(t, cols, "id")].AsInt(); id == 1 {
		t.Error("the filter n >= 20 did not exclude id 1")
	}
	if l, _ := rows[0][colIndex(t, cols, "label")].AsText(); l == "" {
		t.Error("the dynamic output field label is missing")
	}
	// A decay reranker runs in Milvus: allowed read-only.
	_, rows, _ = collect(t, ro, driver.Query{Text: `{"collection":"vecs","annsField":"vec_l2","vector":[1,0,0,0],"limit":3,"rerank":{"inputFields":["id"],"params":{"reranker":"decay","function":"gauss","origin":0,"offset":0,"decay":0.5,"scale":100}}}`})
	if len(rows) != 3 {
		t.Errorf("decay rerank rows %v", rows)
	}
	// A model reranker is egress: refused read-only before the server.
	_, err = ro.Query(ctx, driver.Query{Text: `{"collection":"vecs","annsField":"vec_l2","vector":[1,0,0,0],"rerank":{"inputFields":["label"],"params":{"reranker":"model","provider":"tei","queries":["x"],"endpoint":"http://192.0.2.1"}}}`})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("model rerank (ro) = %v, want ErrReadOnly", err)
	}
}

// A TEXTEMBEDDING function sends query text to its provider. The
// provider here is a fake TEI server on the host, which counts the
// requests it gets: a read-only connection refuses to search the field by
// text and the provider sees nothing; a writable one reaches it.
func TestTextEmbeddingEgress(t *testing.T) {
	var calls atomic.Int64
	// A Linux engine reaches the host through the bridge gateway, not
	// the loopback; Docker Desktop forwards host.docker.internal to it.
	addr := "127.0.0.1:0"
	if runtime.GOOS == "linux" {
		addr = "0.0.0.0:0"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	tei := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Inputs []string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		out := make([][]float32, len(body.Inputs))
		for i := range out {
			out[i] = []float32{1, 0, 0, 0}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	_ = tei.Listener.Close()
	tei.Listener = lis
	tei.Start()
	t.Cleanup(tei.Close)
	endpoint := "http://host.docker.internal:" + strconv.Itoa(lis.Addr().(*net.TCPAddr).Port)
	c := adminT(t)
	ctx := ctx30(t)
	const name = "tobira_textemb"
	sch := entity.NewSchema().WithName(name).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("doc").WithDataType(entity.FieldTypeVarChar).WithMaxLength(512)).
		WithField(entity.NewField().WithName("doc_vec").WithDataType(entity.FieldTypeFloatVector).WithDim(4)).
		WithFunction(entity.NewFunction().WithName("emb").WithType(entity.FunctionTypeTextEmbedding).WithInputFields("doc").WithOutputFields("doc_vec").
			WithParam("provider", "tei").WithParam("endpoint", endpoint))
	if err := c.createCollection(ctx, sch); err != nil {
		t.Fatalf("create a TEXTEMBEDDING collection with a fake TEI provider at %s: %v", endpoint, err)
	}
	t.Cleanup(func() { _ = c.dropCollection(context.Background(), name) })
	if err := c.createIndex(ctx, name, "doc_vec", index.NewAutoIndex(entity.COSINE)); err != nil {
		t.Fatal(err)
	}
	if err := c.insert(ctx, name, column.NewColumnInt64("id", []int64{1}), column.NewColumnVarChar("doc", []string{"hello"})); err != nil {
		t.Fatal(err)
	}
	if err := c.loadCollection(ctx, name); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	ro := open(t, driver.ConnConfig{ReadOnly: true})
	_, err = ro.Similar(ctx, driver.CollectionRef{Name: name}, driver.VectorQuery{Text: "hello", Field: "doc_vec"})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("TEXTEMBEDDING Similar (ro) = %v, want ErrReadOnly", err)
	}
	_, err = ro.Query(ctx, driver.Query{Text: `{"collection":"tobira_textemb","text":"hello","annsField":"doc_vec"}`})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("TEXTEMBEDDING search op (ro) = %v, want ErrReadOnly", err)
	}
	rw := open(t, driver.ConnConfig{})
	_, err = rw.Query(ctx, driver.Query{Text: `{"collection":"tobira_textemb","text":"hello","annsField":"doc_vec"}`, ReadOnly: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("TEXTEMBEDDING search op (Query.ReadOnly) = %v, want ErrReadOnly", err)
	}
	if n := calls.Load() - before; n != 0 {
		t.Errorf("read-only searches reached the embedding provider %d times", n)
	}
	// A vector search of the same field needs no provider: allowed.
	if _, err := ro.Similar(ctx, driver.CollectionRef{Name: name}, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Field: "doc_vec"}); err != nil {
		t.Errorf("vector Similar on a TEXTEMBEDDING field (ro) = %v", err)
	}
	// A writable connection lets Milvus embed the text.
	rs, err := rw.Similar(ctx, driver.CollectionRef{Name: name}, driver.VectorQuery{Text: "hello", Field: "doc_vec"})
	if err != nil {
		t.Fatalf("TEXTEMBEDDING Similar (rw): %v", err)
	}
	_ = rs.Close()
	if calls.Load() == before {
		t.Error("the writable text search did not reach the provider: the test proves nothing")
	}
	sch2, err := ro.Describe(ctx, driver.CollectionRef{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(sch2.Fields, func(f driver.Field) bool { return f.Name == "doc_vec" })
	if i < 0 || !sch2.Fields[i].Generated {
		t.Errorf("doc_vec is not reported as generated: %+v", sch2.Fields)
	}
}

// Dynamic keys that clash with a field or a driver column get a "$meta."
// prefix, and editing that column edits the key, not the field.
func TestDynamicKeys(t *testing.T) {
	scratch(t, "tobira_dyn", true)
	rw := open(t, driver.ConnConfig{})
	ref := driver.CollectionRef{Name: "tobira_dyn"}
	if _, err := rw.Insert(ctx30(t), ref, driver.Object{
		{Key: "id", Value: driver.Int(1)}, {Key: "name", Value: driver.String("static")},
		{Key: "$meta._distance", Value: driver.Int(7)}, {Key: "color", Value: driver.String("red")},
		{Key: "nested", Value: driver.ObjectValue(driver.Member{Key: "a", Value: driver.List(driver.Int(1), driver.Float(2.5))})},
	}); err != nil {
		t.Fatal(err)
	}
	cols, rows, _ := browse(t, rw, ref, driver.BrowseOptions{})
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	if v := rows[0][colIndex(t, cols, "$meta._distance")]; !drivertest.EqualValue(driver.Int(7), v) {
		t.Errorf("$meta._distance = %s", v)
	}
	if v := rows[0][colIndex(t, cols, "color")]; !drivertest.EqualValue(driver.String("red"), v) {
		t.Errorf("color = %s", v)
	}
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	if err := rw.Update(ctx30(t), ref, key, driver.Object{{Key: "$meta._distance", Value: driver.Int(8)}, {Key: "name", Value: driver.Null()}}); err != nil {
		t.Fatal(err)
	}
	cols, rows, _ = browse(t, rw, ref, driver.BrowseOptions{})
	if v := rows[0][colIndex(t, cols, "$meta._distance")]; !drivertest.EqualValue(driver.Int(8), v) {
		t.Errorf("after update $meta._distance = %s", v)
	}
	if v := rows[0][colIndex(t, cols, "color")]; !drivertest.EqualValue(driver.String("red"), v) {
		t.Errorf("an update of one dynamic key lost another: color = %s", v)
	}
	if v := rows[0][colIndex(t, cols, "name")]; !v.IsNull() {
		t.Errorf("name = %s, want NULL", v)
	}
	// Similar results put _distance first; the dynamic key keeps its
	// prefix.
	rs, err := rw.Similar(ctx30(t), ref, driver.VectorQuery{Vector: []float32{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	scols := rs.Columns()
	_ = rs.Close()
	n := 0
	for _, c := range scols {
		if c.Name == distanceColumn {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Similar columns %v: want exactly one _distance", scols)
	}
	// Filtering and sorting by dynamic keys.
	_, rows, _ = browse(t, rw, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "color", Op: driver.OpEq, Value: driver.String("red")}}}})
	if len(rows) != 1 {
		t.Errorf("filter on a dynamic key: %d rows", len(rows))
	}
	_, rows, _ = browse(t, rw, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "$meta._distance", Op: driver.OpGt, Value: driver.Int(7)}}}})
	if len(rows) != 1 {
		t.Errorf("filter on a prefixed dynamic key: %d rows", len(rows))
	}
}

func TestMutatorSemantics(t *testing.T) {
	scratch(t, "tobira_mut", true)
	rw := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	ref := driver.CollectionRef{Name: "tobira_mut"}
	key := driver.Object{{Key: "id", Value: driver.Int(5)}}
	if _, err := rw.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(5)}, {Key: "vec", Value: driver.List(driver.Int(1), driver.Int(2))}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(5)}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("duplicate key insert = %v, want ErrConflict", err)
	}
	if _, err := rw.Insert(ctx, ref, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("insert without a key = %v, want ErrConstraint", err)
	}
	if _, err := rw.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(6)}, {Key: "vec", Value: driver.List(driver.Int(1))}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("wrong vector dimension = %v, want ErrSyntax", err)
	}
	if err := rw.Update(ctx, ref, key, driver.Object{{Key: "vec", Value: driver.Vector([]float32{3, 4})}, {Key: "name", Value: driver.String("five")}}); err != nil {
		t.Fatal(err)
	}
	cols, rows, _ := collect(t, rw, driver.Query{Text: `{"op":"get","collection":"tobira_mut","ids":[5]}`})
	if len(rows) != 1 {
		t.Fatalf("get rows %v", rows)
	}
	if v := rows[0][colIndex(t, cols, "vec")]; !drivertest.EqualValue(driver.Vector([]float32{3, 4}), v) {
		t.Errorf("vec after update = %s", v)
	}
	if err := rw.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(404)}}, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("update of a missing key = %v", err)
	}
	if err := rw.Delete(ctx, ref, key); err != nil {
		t.Fatal(err)
	}
	if err := rw.Delete(ctx, ref, key); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}

	// An auto-id collection returns the key Milvus assigns.
	c := adminT(t)
	sch := entity.NewSchema().WithName("tobira_auto").
		WithField(entity.NewField().WithName("pk").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true).WithIsAutoID(true)).
		WithField(entity.NewField().WithName("v").WithDataType(entity.FieldTypeFloatVector).WithDim(2))
	if err := createIndexed(ctx, c, sch, map[string]entity.MetricType{"v": entity.L2}, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.dropCollection(context.Background(), "tobira_auto")
	})
	got, err := rw.Insert(ctx, driver.CollectionRef{Name: "tobira_auto"}, driver.Object{{Key: "v", Value: driver.Vector([]float32{1, 1})}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "pk" || got[0].Value.Type() != driver.TypeInt {
		t.Errorf("auto-id key = %v", got)
	}
	if _, err := rw.Insert(ctx, driver.CollectionRef{Name: "tobira_auto"}, driver.Object{{Key: "pk", Value: driver.Int(1)}, {Key: "v", Value: driver.Vector([]float32{1, 1})}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("a key for an auto-id field = %v, want ErrUnsupported", err)
	}
	// Milvus 2.6 gives an upserted auto-id entity a new key
	// (checked here: a partial upsert of key k left key k+1), so Update
	// refuses auto-id collections and leaves the entity as it was.
	autoKey := driver.Object{got[0]}
	if err := rw.Update(ctx, driver.CollectionRef{Name: "tobira_auto"}, autoKey, driver.Object{{Key: "v", Value: driver.Vector([]float32{2, 3})}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Update on an auto-id collection = %v, want ErrUnsupported", err)
	}
	k, _ := got[0].Value.AsInt()
	acols, arows, _ := collect(t, rw, driver.Query{Text: `{"collection":"tobira_auto"}`})
	if len(arows) != 1 {
		t.Fatalf("the auto-id collection holds %d entities, want 1", len(arows))
	}
	if n, _ := arows[0][colIndex(t, acols, "pk")].AsInt(); n != k {
		t.Errorf("the auto-id key changed from %d to %d", k, n)
	}
}

func TestQueryOps(t *testing.T) {
	ro := open(t, driver.ConnConfig{ReadOnly: true})
	_, rows, _ := collect(t, ro, driver.Query{Text: `{"op":"count","collection":"tobira_people","filter":"active == {a}","params":{"a":true}}`})
	want := 0
	for _, r := range drivertest.People.Rows {
		if b, _ := r[5].AsBool(); b {
			want++
		}
	}
	if len(rows) != 1 || !drivertest.EqualValue(driver.Int(int64(want)), rows[0][0]) {
		t.Errorf("count = %v, want %d", rows, want)
	}
	cols, rows, _ := collect(t, ro, driver.Query{Text: `{"collection":"tobira_people","filter":"age > 30","outputFields":["name"],"limit":5,"offset":2}`})
	if len(rows) != 5 || len(cols) != 2 || cols[0].Name != "id" || cols[1].Name != "name" {
		t.Errorf("query with output fields: cols %v rows %d", cols, len(rows))
	}
	_, rows, st := collect(t, ro, driver.Query{Text: `{"collection":"tobira_people"}`, Limit: 10})
	if len(rows) != 10 || !st.Truncated {
		t.Errorf("Query.Limit: %d rows, truncated %v", len(rows), st.Truncated)
	}
	_, rows, _ = collect(t, ro, driver.Query{Text: `{"op":"get","collection":"tobira_people","ids":[3,1,999]}`})
	if len(rows) != 2 {
		t.Errorf("get rows = %d", len(rows))
	}
	_, rows, _ = collect(t, ro, driver.Query{Text: `{"collection":"tobira_people","filter":"name like {p}","params":{"p":"Ada%"}}`})
	if len(rows) == 0 {
		t.Error("a like template matched nothing")
	}
	// A filter the server cannot parse is ErrSyntax.
	_, err := ro.Query(ctx30(t), driver.Query{Text: `{"collection":"tobira_people","filter":"nosuchfield > 1 and"}`})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad filter = %v, want ErrSyntax", err)
	}
}

func TestBrowseSortedAndPaging(t *testing.T) {
	ro := open(t, driver.ConnConfig{ReadOnly: true})
	// The key descending, sorted by tobira, in pages.
	var ids []int64
	cursor := ""
	for range 10 {
		cols, rows, st := browse(t, ro, people, driver.BrowseOptions{Limit: 25, Cursor: cursor, Sort: []driver.SortKey{{Field: "id", Desc: true}}})
		id := colIndex(t, cols, "id")
		for _, r := range rows {
			n, _ := r[id].AsInt()
			ids = append(ids, n)
		}
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if len(ids) != 57 || ids[0] != 57 || ids[56] != 1 {
		t.Errorf("id desc pages: %d ids, first %d", len(ids), ids[0])
	}
	// A cursor reused with another sort is refused.
	_, _, st := browse(t, ro, people, driver.BrowseOptions{Limit: 5, Sort: []driver.SortKey{{Field: "age"}}})
	if _, err := ro.Browse(ctx30(t), people, driver.BrowseOptions{Cursor: st.NextCursor}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("cursor reuse = %v", err)
	}
	// A raw filter.
	_, rows, _ := browse(t, ro, people, driver.BrowseOptions{Filter: driver.Filter{Raw: "age is null"}})
	if len(rows) != 5 {
		t.Errorf("raw filter age is null: %d rows, want 5", len(rows))
	}
}

func TestDescribeAndCatalog(t *testing.T) {
	ro := open(t, driver.ConnConfig{ReadOnly: true})
	sch, err := ro.Describe(ctx30(t), driver.CollectionRef{Name: vecsColl})
	if err != nil {
		t.Fatal(err)
	}
	if sch.RowEstimate < 0 || len(sch.Indexes) != 3 || sch.DDL == "" {
		t.Errorf("Describe(vecs) = rows %d, indexes %v", sch.RowEstimate, sch.Indexes)
	}
	var names []string
	for _, f := range sch.Fields {
		names = append(names, f.Name)
	}
	for _, want := range []string{"id", "vec_l2", "label", "n"} {
		if !slices.Contains(names, want) {
			t.Errorf("Describe(vecs) lacks %q: %v", want, names)
		}
	}
	// A second database.
	c := adminT(t)
	ctx := ctx30(t)
	if err := c.createDatabase(ctx, "tobira_db2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.dropDatabase(context.Background(), "tobira_db2") })
	nss, err := ro.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(nss, func(n driver.Namespace) bool { return n.Name == "tobira_db2" }) {
		t.Errorf("Namespaces = %v", nss)
	}
	cols, err := ro.Collections(ctx, driver.NamespaceRef{Name: "tobira_db2"})
	if err != nil || len(cols) != 0 {
		t.Errorf("Collections(tobira_db2) = %v, %v", cols, err)
	}
	if _, err := ro.Describe(ctx, driver.CollectionRef{Namespace: "tobira_db2", Name: peopleColl}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Describe in another database = %v, want ErrNotFound", err)
	}
	// A DSN naming a missing database fails at Open.
	cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if c, err := New().Open(cctx, driver.ConnConfig{DSN: "milvus://root:Milvus@" + testAddr + "/nosuchdb"}); err == nil {
		_ = c.Close()
		t.Error("Open with a missing database succeeded")
	}
}

func TestCancellation(t *testing.T) {
	rw := open(t, driver.ConnConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, err := range map[string]error{
		"query": func() error {
			_, err := rw.Query(ctx, driver.Query{Text: `{"collection":"tobira_people"}`})
			return err
		}(),
		"browse": func() error { _, err := rw.Browse(ctx, people, driver.BrowseOptions{}); return err }(),
		"similar": func() error {
			_, err := rw.Similar(ctx, driver.CollectionRef{Name: vecsColl}, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Metric: driver.MetricL2})
			return err
		}(),
		"describe": func() error { _, err := rw.Describe(ctx, people); return err }(),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a cancelled ctx = %v", name, err)
		}
	}
	if err := rw.Ping(ctx30(t)); err != nil {
		t.Errorf("the conn is unusable after cancellations: %v", err)
	}
}
