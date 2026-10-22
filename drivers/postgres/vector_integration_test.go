//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

var vectorDBSeq atomic.Int64

// vectorDB creates a database with the pgvector extension in extSchema
// ("" = the default, public) and the drivertest vector dataset seeded as
// table vecs, and returns its DSN. The database is dropped on cleanup.
func vectorDB(t *testing.T, extSchema string) string {
	t.Helper()
	name := fmt.Sprintf("tobira_vec_%d", vectorDBSeq.Add(1))
	dsn := createDB(t, name)
	vecType := "vector"
	stmts := []string{}
	if extSchema != "" {
		stmts = append(stmts, "CREATE SCHEMA "+extSchema, "CREATE EXTENSION vector SCHEMA "+extSchema)
		vecType = extSchema + ".vector"
	} else {
		stmts = append(stmts, "CREATE EXTENSION vector")
	}
	execOn(t, dsn, stmts...)
	seedVectors(t, dsn, "vecs", vecType)
	return dsn
}

// createDB creates database name on the shared container and returns its DSN.
func createDB(t *testing.T, name string) string {
	t.Helper()
	execOn(t, testDSN, "CREATE DATABASE "+name)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), testDSN)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// execOn runs each statement on a fresh connection to dsn.
func execOn(t testing.TB, dsn string, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	for _, s := range stmts {
		if _, err := c.Exec(ctx, s); err != nil {
			t.Fatalf("%.80s: %v", s, err)
		}
	}
}

// seedVectors creates table (id integer primary key, vec <vecType>(4),
// payload jsonb) and loads drivertest.VectorDataset into it.
func seedVectors(t testing.TB, dsn, table, vecType string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	if _, err := c.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (id integer PRIMARY KEY, vec %s(4), payload jsonb)", table, vecType)); err != nil {
		t.Fatal(err)
	}
	for _, r := range drivertest.VectorDataset().Rows {
		id, _ := r[0].AsInt()
		vec, err := paramToPG(r[1])
		if err != nil {
			t.Fatal(err)
		}
		obj, err := paramToPG(r[2])
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES ($1, CAST($2 AS %s), $3)", table, vecType), id, vec, payload); err != nil {
			t.Fatal(err)
		}
	}
}

// openDSN opens dsn with cfg's other fields and checks the pool is idle on cleanup.
func openDSN(t *testing.T, dsn string, cfg driver.ConnConfig) *conn {
	t.Helper()
	cfg.DSN = dsn
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	pc := c.(*conn)
	t.Cleanup(func() {
		requireIdle(t, pc)
		_ = c.Close()
	})
	return pc
}

var vecsRef = driver.CollectionRef{Namespace: "public", Name: "vecs"}

// similar runs Similar and returns the columns and every row.
func similar(t *testing.T, c *conn, ref driver.CollectionRef, q driver.VectorQuery) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Similar(ctxT(t), ref, q)
	if err != nil {
		t.Fatalf("Similar(%+v): %v", q, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return cols, rows, st
}

func ids(t *testing.T, cols []driver.Column, rows []driver.Row) []int64 {
	t.Helper()
	i := colIdx(t, cols, "id")
	out := make([]int64, len(rows))
	for j, r := range rows {
		out[j], _ = r[i].AsInt()
	}
	return out
}

func TestVectorCapabilityFollowsExtension(t *testing.T) {
	plain := openPG(t, false) // the shared database has no vector extension
	if _, ok := driver.As[driver.VectorSearcher](plain); ok {
		t.Error("VectorSearcher offered on a database without pgvector")
	}
	if _, err := plain.Similar(ctxT(t), vecsRef, driver.VectorQuery{Vector: []float32{1}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Similar without pgvector: %v, want ErrUnsupported", err)
	}
	vc := openDSN(t, vectorDB(t, ""), driver.ConnConfig{})
	if _, ok := driver.As[driver.VectorSearcher](vc); !ok {
		t.Error("VectorSearcher not offered on a database with pgvector")
	}
	if vc.vectorSchema != "public" {
		t.Errorf("vectorSchema = %q", vc.vectorSchema)
	}
}

func TestVectorValuesRoundTrip(t *testing.T) {
	dsn := vectorDB(t, "")
	c := openDSN(t, dsn, driver.ConnConfig{})
	rows, _, cols := collect(t, c, driver.Query{Text: "select vec from vecs where id = 1"})
	if cols[0].Type != driver.TypeVector {
		t.Errorf("column type = %s, want vector", cols[0].Type)
	}
	if !drivertest.EqualValue(driver.Vector([]float32{0.9, 0.3, 0, 0}), rows[0][0]) {
		t.Errorf("vec = %s", rows[0][0])
	}
	rows, _, _ = collect(t, c, driver.Query{Text: "select cast($1 as vector)", Params: []driver.Value{driver.Vector([]float32{1.5, -2, 0.25})}})
	if !drivertest.EqualValue(driver.Vector([]float32{1.5, -2, 0.25}), rows[0][0]) {
		t.Errorf("param round trip = %s", rows[0][0])
	}

	schema, err := c.Describe(ctxT(t), vecsRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range schema.Fields {
		if f.Name == "vec" && (f.Type != driver.TypeVector || f.NativeType != "vector(4)") {
			t.Errorf("vec field = %+v", f)
		}
	}

	if _, err := c.Insert(ctxT(t), vecsRef, driver.Object{
		{Key: "id", Value: driver.Int(100)},
		{Key: "vec", Value: driver.Vector([]float32{0.5, 0.5, 0.5, 0.5})},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := c.Update(ctxT(t), vecsRef, driver.Object{{Key: "id", Value: driver.Int(100)}}, driver.Object{
		{Key: "vec", Value: driver.Vector([]float32{0.25, 0, 0, 1})},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	rows, _, _ = collect(t, c, driver.Query{Text: "select vec from vecs where id = 100"})
	if !drivertest.EqualValue(driver.Vector([]float32{0.25, 0, 0, 1}), rows[0][0]) {
		t.Errorf("after Update vec = %s", rows[0][0])
	}
	// The binary form decodes too.
	rows, _, _ = collect(t, c, driver.Query{Text: "select vector_send(vec) from vecs where id = 100"})
	b, _ := rows[0][0].AsBytes()
	got, err := vectorFromPG(1, b)
	if err != nil || !drivertest.EqualValue(driver.Vector([]float32{0.25, 0, 0, 1}), got) {
		t.Errorf("binary decode = %s, %v", got, err)
	}
}

func TestSimilarMetrics(t *testing.T) {
	c := openDSN(t, vectorDB(t, ""), driver.ConnConfig{})
	q := []float32{1, 0, 0, 0}
	tests := []struct {
		metric   driver.VectorMetric
		wantTop  int64
		wantDist float64 // _distance of the top row
	}{
		{driver.MetricL2, 1, math.Sqrt(0.1*0.1 + 0.3*0.3)},
		{driver.MetricCosine, 2, 1 - 5/math.Sqrt(25+0.01)},
		{driver.MetricInnerProduct, 3, -6},
		{driver.MetricDefault, 2, 1 - 5/math.Sqrt(25+0.01)}, // cosine without an index
	}
	for _, tt := range tests {
		cols, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Vector: q, K: 3, Metric: tt.metric})
		if len(rows) != 3 {
			t.Fatalf("%q: %d rows, want 3", tt.metric, len(rows))
		}
		if got := ids(t, cols, rows)[0]; got != tt.wantTop {
			t.Errorf("%q: top id %d, want %d", tt.metric, got, tt.wantTop)
		}
		di := colIdx(t, cols, "_distance")
		if cols[di].Type != driver.TypeFloat || di != len(cols)-1 {
			t.Errorf("%q: _distance column %+v at %d", tt.metric, cols[di], di)
		}
		d, _ := rows[0][di].AsFloat()
		if math.Abs(d-tt.wantDist) > 1e-5 {
			t.Errorf("%q: _distance %g, want %g", tt.metric, d, tt.wantDist)
		}
		vi := colIdx(t, cols, "vec")
		if _, ok := rows[0][vi].AsVector(); !ok {
			t.Errorf("%q: vec is %s, want a vector", tt.metric, rows[0][vi].Type())
		}
	}
	if _, err := c.Similar(ctxT(t), vecsRef, driver.VectorQuery{Vector: q, Metric: "l1"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("metric l1: %v, want ErrUnsupported", err)
	}
}

func TestSimilarErrors(t *testing.T) {
	c := openDSN(t, vectorDB(t, ""), driver.ConnConfig{})
	tests := []struct {
		name string
		ref  driver.CollectionRef
		q    driver.VectorQuery
		kind error
	}{
		{"missing table", driver.CollectionRef{Namespace: "public", Name: "nope"}, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}}, driver.ErrNotFound},
		{"not a vector column", vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Field: "payload"}, driver.ErrUnsupported},
		{"dimension mismatch", vecsRef, driver.VectorQuery{Vector: []float32{1, 0}}, driver.ErrSyntax},
		{"text without embedder", vecsRef, driver.VectorQuery{Text: "hello"}, driver.ErrUnsupported},
	}
	for _, tt := range tests {
		if _, err := c.Similar(ctxT(t), tt.ref, tt.q); !errors.Is(err, tt.kind) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.kind)
		}
	}
}

func TestSimilarKIsCapped(t *testing.T) {
	dsn := vectorDB(t, "")
	execOn(t, dsn, "CREATE TABLE many AS SELECT g AS id, CAST('['||g||',1]' AS vector(2)) AS vec FROM generate_series(1, 1100) g")
	c := openDSN(t, dsn, driver.ConnConfig{})
	ref := driver.CollectionRef{Namespace: "public", Name: "many"}
	_, rows, _ := similar(t, c, ref, driver.VectorQuery{Vector: []float32{1, 1}, K: 5000, Metric: driver.MetricL2})
	if len(rows) != maxSimilarK {
		t.Errorf("K 5000 returned %d rows, want the cap %d", len(rows), maxSimilarK)
	}
	_, rows, _ = similar(t, c, ref, driver.VectorQuery{Vector: []float32{1, 1}})
	if len(rows) != defaultSimilarK {
		t.Errorf("K 0 returned %d rows, want %d", len(rows), defaultSimilarK)
	}
}

func TestSimilarFilter(t *testing.T) {
	c := openDSN(t, vectorDB(t, ""), driver.ConnConfig{})
	q := []float32{1, 0, 0, 0}
	cols, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Vector: q, K: 2, Metric: driver.MetricL2, Filter: driver.Filter{
		Conditions: []driver.Condition{{Field: "id", Op: driver.OpNe, Value: driver.Int(1)}},
	}})
	if got := ids(t, cols, rows); len(got) != 2 || got[0] != 4 || got[1] != 6 {
		t.Errorf("id <> 1: ids %v, want [4 6]", got)
	}
	cols, rows, _ = similar(t, c, vecsRef, driver.VectorQuery{Vector: q, Metric: driver.MetricL2, Filter: driver.Filter{Raw: "payload->>'label' = 'large'"}})
	if got := ids(t, cols, rows); len(got) != 1 || got[0] != 3 {
		t.Errorf("raw filter: ids %v, want [3]", got)
	}
}

// TestSimilarRunsReadOnly proves the search takes the read-only path on a
// read-write connection: a filter that would write fails with ErrReadOnly
// and changes nothing.
func TestSimilarRunsReadOnly(t *testing.T) {
	dsn := vectorDB(t, "")
	execOn(t, dsn, "CREATE SEQUENCE vec_seq", "CREATE TABLE vec_log (n integer)",
		"CREATE FUNCTION vec_write() RETURNS boolean LANGUAGE sql AS 'INSERT INTO vec_log VALUES (1); SELECT true'")
	c := openDSN(t, dsn, driver.ConnConfig{})
	for _, raw := range []string{"nextval('vec_seq') > 0", "vec_write()"} {
		rs, err := c.Similar(ctxT(t), vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Filter: driver.Filter{Raw: raw}})
		if err == nil { // the error may surface on the first row
			_, _, err = driver.Collect(ctxT(t), rs, 0)
		}
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("filter %q: %v, want ErrReadOnly", raw, err)
		}
	}
	rows, _, _ := collect(t, c, driver.Query{Text: "select (select is_called from vec_seq), (select count(*) from vec_log)"})
	if called, _ := rows[0][0].AsBool(); called {
		t.Error("the sequence advanced")
	}
	if n, _ := rows[0][1].AsInt(); n != 0 {
		t.Errorf("vec_log has %d rows", n)
	}
	// A read-only connection searches too.
	ro := openDSN(t, dsn, driver.ConnConfig{ReadOnly: true})
	if _, rows, _ := similar(t, ro, vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, K: 1}); len(rows) != 1 {
		t.Errorf("read-only connection: %d rows", len(rows))
	}
}

type errEmbedder struct{ err error }

func (e errEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, e.err }
func (errEmbedder) Dimensions() int                                        { return 0 }

func TestSimilarText(t *testing.T) {
	dsn := vectorDB(t, "")
	emb := drivertest.HashEmbedder{Dims: 4}
	c := openDSN(t, dsn, driver.ConnConfig{Embedder: emb})
	vecs, err := emb.Embed(ctxT(t), []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	cols, byText, _ := similar(t, c, vecsRef, driver.VectorQuery{Text: "hello", K: 6, Metric: driver.MetricL2})
	cols2, byVec, _ := similar(t, c, vecsRef, driver.VectorQuery{Vector: vecs[0], K: 6, Metric: driver.MetricL2})
	if a, b := ids(t, cols, byText), ids(t, cols2, byVec); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("text ranking %v differs from its embedding's %v", a, b)
	}

	bad := openDSN(t, dsn, driver.ConnConfig{Embedder: errEmbedder{driver.Wrap(driver.ErrAuth, "ai", "embed", errors.New("bad key"))}})
	if _, err := bad.Similar(ctxT(t), vecsRef, driver.VectorQuery{Text: "hello"}); !errors.Is(err, driver.ErrAuth) {
		t.Errorf("failing embedder: %v, want its ErrAuth", err)
	}
	wrong := openDSN(t, dsn, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 3}})
	if _, err := wrong.Similar(ctxT(t), vecsRef, driver.VectorQuery{Text: "hello"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("3-dim embedder on vector(4): %v, want ErrSyntax", err)
	}
}

func TestSimilarExtensionOutsideSearchPath(t *testing.T) {
	c := openDSN(t, vectorDB(t, "vext"), driver.ConnConfig{})
	if c.vectorSchema != "vext" {
		t.Fatalf("vectorSchema = %q", c.vectorSchema)
	}
	for metric, want := range map[driver.VectorMetric]int64{driver.MetricL2: 1, driver.MetricCosine: 2, driver.MetricInnerProduct: 3} {
		cols, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, K: 1, Metric: metric})
		if got := ids(t, cols, rows); len(got) != 1 || got[0] != want {
			t.Errorf("%q: ids %v, want [%d]", metric, got, want)
		}
		vi := colIdx(t, cols, "vec")
		if _, ok := rows[0][vi].AsVector(); !ok {
			t.Errorf("vec is %s, want a vector", rows[0][vi].Type())
		}
	}
}

func TestSimilarDefaultMetricFollowsIndex(t *testing.T) {
	dsn := vectorDB(t, "")
	execOn(t, dsn, "CREATE INDEX vecs_ip ON vecs USING hnsw (vec vector_ip_ops)")
	c := openDSN(t, dsn, driver.ConnConfig{})
	cols, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, K: 1})
	if got := ids(t, cols, rows); len(got) != 1 || got[0] != 3 {
		t.Errorf("default metric with an ip index: ids %v, want [3] (inner product)", got)
	}
	// An explicit metric still wins.
	cols, rows, _ = similar(t, c, vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, K: 1, Metric: driver.MetricL2})
	if got := ids(t, cols, rows); len(got) != 1 || got[0] != 1 {
		t.Errorf("explicit l2: ids %v, want [1]", got)
	}
}

func TestSimilarDistanceColumnClash(t *testing.T) {
	dsn := vectorDB(t, "")
	execOn(t, dsn, "CREATE TABLE clash (id integer PRIMARY KEY, vec vector(2), _distance text)",
		"INSERT INTO clash VALUES (1, '[1,0]', 'mine'), (2, '[0,1]', 'also mine')")
	c := openDSN(t, dsn, driver.ConnConfig{})
	cols, rows, _ := similar(t, c, driver.CollectionRef{Namespace: "public", Name: "clash"}, driver.VectorQuery{Vector: []float32{1, 0}, Metric: driver.MetricL2})
	if last := cols[len(cols)-1]; last.Name != "_distance_2" || last.Type != driver.TypeFloat {
		t.Fatalf("distance column = %+v, want _distance_2 float", last)
	}
	if got := rows[0][colIdx(t, cols, "_distance")].String(); got != "mine" {
		t.Errorf("the table's own _distance = %q", got)
	}
	if d, _ := rows[0][len(cols)-1].AsFloat(); d != 0 {
		t.Errorf("_distance_2 = %g, want 0", d)
	}
}

// TestSimilarCachesUntilInvalidate shows the schema and index metric are
// cached per connection, and that Invalidate (:reload) drops them.
func TestSimilarCachesUntilInvalidate(t *testing.T) {
	dsn := vectorDB(t, "")
	c := openDSN(t, dsn, driver.ConnConfig{})
	top := func() int64 {
		cols, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, K: 1})
		return ids(t, cols, rows)[0]
	}
	if got := top(); got != 2 {
		t.Fatalf("default metric without an index: top %d, want 2 (cosine)", got)
	}
	execOn(t, dsn, "CREATE INDEX vecs_ip ON vecs USING hnsw (vec vector_ip_ops)")
	if got := top(); got != 2 {
		t.Errorf("within the TTL: top %d, want the cached cosine answer 2", got)
	}
	c.Invalidate()
	if got := top(); got != 3 {
		t.Errorf("after Invalidate: top %d, want 3 (the new ip index)", got)
	}
}
