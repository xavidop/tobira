//go:build integration

package qdrant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

var vecsRef = driver.CollectionRef{Namespace: defaultNamespace, Name: vecsColl}

func similar(t *testing.T, c *conn, ref driver.CollectionRef, q driver.VectorQuery) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	ctx := ctx30(t)
	rs, err := c.Similar(ctx, ref, q)
	if err != nil {
		t.Fatalf("Similar: %v", err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return cols, rows, st
}

// TestSimilarDistances checks _distance against the exact value of each
// metric for the vector fixture and the query (1,0,0,0).
func TestSimilarDistances(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	q := []float32{1, 0, 0, 0}
	data := map[int64][]float32{}
	for _, r := range drivertest.VectorDataset().Rows {
		id, _ := r[0].AsInt()
		data[id], _ = r[1].AsVector()
	}
	exact := map[driver.VectorMetric]func(v []float32) float64{
		driver.MetricL2: func(v []float32) float64 {
			s := 0.0
			for i := range v {
				d := float64(v[i] - q[i])
				s += d * d
			}
			return math.Sqrt(s)
		},
		driver.MetricCosine: func(v []float32) float64 {
			n := 0.0
			for _, x := range v {
				n += float64(x) * float64(x)
			}
			return 1 - float64(v[0])/math.Sqrt(n)
		},
		driver.MetricInnerProduct: func(v []float32) float64 { return -float64(v[0]) },
	}
	for metric, fn := range exact {
		cols, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Vector: q, K: 6, Metric: metric})
		if len(rows) != 6 {
			t.Fatalf("%s: %d rows", metric, len(rows))
		}
		id, dist := colIndex(t, cols, "id"), colIndex(t, cols, "_distance")
		for _, name := range []string{"vec_l2", "vec_cos", "vec_ip", "label", "n"} {
			colIndex(t, cols, name)
		}
		for _, r := range rows {
			n, _ := r[id].AsInt()
			got, _ := r[dist].AsFloat()
			if want := fn(data[n]); math.Abs(got-want) > 1e-4 {
				t.Errorf("%s: id %d _distance %v, want %v", metric, n, got, want)
			}
		}
	}
}

func TestSimilarErrors(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	for _, tt := range []struct {
		name string
		ref  driver.CollectionRef
		q    driver.VectorQuery
		kind error
	}{
		{"ambiguous field", vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}}, driver.ErrSyntax},
		{"wrong metric for field", vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Field: "vec_l2", Metric: driver.MetricCosine}, driver.ErrUnsupported},
		{"missing field", vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Field: "nope"}, driver.ErrNotFound},
		{"dimensions", vecsRef, driver.VectorQuery{Vector: []float32{1, 0}, Field: "vec_l2"}, driver.ErrSyntax},
		{"missing collection", driver.CollectionRef{Namespace: defaultNamespace, Name: "nope"}, driver.VectorQuery{Vector: []float32{1}}, driver.ErrNotFound},
		{"no vectors", driver.CollectionRef{Namespace: defaultNamespace, Name: peopleColl}, driver.VectorQuery{Vector: []float32{1}}, driver.ErrUnsupported},
		{"bad namespace", driver.CollectionRef{Namespace: "other", Name: vecsColl}, driver.VectorQuery{Vector: []float32{1}}, driver.ErrNotFound},
		{"text without embedder", vecsRef, driver.VectorQuery{Text: "hello", Field: "vec_l2"}, driver.ErrUnsupported},
	} {
		_, err := c.Similar(ctx, tt.ref, tt.q)
		if !errors.Is(err, tt.kind) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.kind)
		}
	}
}

// TestSimilarText embeds the text with the host embedder; an embedding of
// the wrong size is a syntax error naming both sizes.
func TestSimilarText(t *testing.T) {
	c := open(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 4}})
	_, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Text: "near", Field: "vec_cos", K: 2})
	if len(rows) != 2 {
		t.Errorf("text search: %d rows, want 2", len(rows))
	}
	bad := open(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 8}})
	_, err := bad.Similar(ctx30(t), vecsRef, driver.VectorQuery{Text: "near", Field: "vec_cos"})
	if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), "8 dimensions") {
		t.Errorf("wrong embedding size: err = %v", err)
	}
}

// TestSimilarKCap: k above the cap returns at most maxK points with a
// warning; k = 0 returns the default.
func TestSimilarKCap(t *testing.T) {
	coll := "tobira_kcap"
	admin := createColl(t, coll, 2, pb.Distance_Euclid)
	var pts []*pb.PointStruct
	for i := range maxK + 50 {
		pts = append(pts, &pb.PointStruct{Id: pb.NewIDNum(uint64(i + 1)), Vectors: pb.NewVectorsDense([]float32{float32(i), 1})})
	}
	wait := true
	if _, err := admin.Upsert(ctx30(t), &pb.UpsertPoints{CollectionName: coll, Wait: &wait, Points: pts}); err != nil {
		t.Fatal(err)
	}
	c := open(t, driver.ConnConfig{})
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: coll}
	_, rows, st := similar(t, c, ref, driver.VectorQuery{Vector: []float32{0, 1}, K: 1 << 30})
	if len(rows) != maxK || len(st.Warnings) != 1 {
		t.Errorf("k = 2^30: %d rows, warnings %q; want %d rows and one warning", len(rows), st.Warnings, maxK)
	}
	_, rows, _ = similar(t, c, ref, driver.VectorQuery{Vector: []float32{0, 1}})
	if len(rows) != defaultK {
		t.Errorf("k = 0: %d rows, want %d", len(rows), defaultK)
	}
	_, rows, st = collect(t, c, driver.Query{Text: `{"op":"query","collection":"tobira_kcap","query":{"nearest":{"dense":{"data":[0,1]}}},"limit":"5000"}`})
	if len(rows) != maxK || len(st.Warnings) != 1 {
		t.Errorf("query limit 5000: %d rows, warnings %q", len(rows), st.Warnings)
	}
	// Filter on the id narrows the search.
	_, rows, _ = similar(t, c, ref, driver.VectorQuery{Vector: []float32{0, 1}, K: 5,
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(7), driver.Int(9))}}}})
	if len(rows) != 2 {
		t.Errorf("filtered search: %d rows, want 2", len(rows))
	}
}

func TestQueryOps(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	for _, tt := range []struct {
		text     string
		rows     int
		firstCol string
		check    func(t *testing.T, cols []driver.Column, rows []driver.Row)
	}{
		{`{"op":"count","collection":"tobira_people","exact":true}`, 1, "count", func(t *testing.T, _ []driver.Column, rows []driver.Row) {
			if n, _ := rows[0][0].AsInt(); n != int64(len(drivertest.People.Rows)) {
				t.Errorf("count = %d", n)
			}
		}},
		{`{"op":"retrieve","collection":"tobira_people","ids":[{"num":"3"},{"num":"1"}]}`, 2, "id", nil},
		{`{"op":"scroll","collection":"tobira_people","limit":5}`, 5, "id", nil},
		{`{"op":"scroll","collection":"tobira_people","limit":300}`, len(drivertest.People.Rows), "id", nil},
		{`{"op":"scroll","collection":"tobira_people","filter":{"must":[{"field":{"key":"active","match":{"boolean":false}}}]}}`, 19, "id", nil},
		{`{"op":"scroll","collection":"tobira_people","order_by":{"key":"age","direction":"Desc"},"limit":3}`, 3, "id", func(t *testing.T, cols []driver.Column, rows []driver.Row) {
			if a, _ := rows[0][colIndex(t, cols, "age")].AsInt(); a != 63 {
				t.Errorf("oldest age = %d, want 63", a)
			}
		}},
		{`{"op":"search","collection":"vecs","vector":[1,0,0,0],"vector_name":"vec_l2","limit":2}`, 2, "id", func(t *testing.T, cols []driver.Column, rows []driver.Row) {
			if id, _ := rows[0][0].AsInt(); id != 1 {
				t.Errorf("nearest = %d", id)
			}
			colIndex(t, cols, "_score")
		}},
		{`{"op":"query","collection":"vecs","query":{"nearest":{"dense":{"data":[1,0,0,0]}}},"using":"vec_ip","limit":1,"with_vectors":{"enable":true}}`, 1, "id", func(t *testing.T, cols []driver.Column, rows []driver.Row) {
			if id, _ := rows[0][0].AsInt(); id != 3 {
				t.Errorf("ip nearest = %d", id)
			}
			if v := rows[0][colIndex(t, cols, "vec_ip")]; v.Type() != driver.TypeVector {
				t.Errorf("vector column = %s", v)
			}
		}},
	} {
		cols, rows, _ := collect(t, c, driver.Query{Text: tt.text})
		if len(rows) != tt.rows || cols[0].Name != tt.firstCol {
			t.Errorf("%s: %d rows, first column %q; want %d, %q", tt.text, len(rows), cols[0].Name, tt.rows, tt.firstCol)
			continue
		}
		if tt.check != nil {
			tt.check(t, cols, rows)
		}
	}
	// Query.Limit caps a scroll and reports truncation.
	_, rows, st := collect(t, c, driver.Query{Text: `{"op":"scroll","collection":"tobira_people"}`, Limit: 7})
	if len(rows) != 7 || !st.Truncated {
		t.Errorf("Limit 7: %d rows, truncated %v", len(rows), st.Truncated)
	}
	for _, bad := range []struct {
		text string
		kind error
	}{
		{`{"op":"scroll","collection":"tobira_people","filter":{"must":[{"bogus":1}]}}`, driver.ErrSyntax},
		{`{"op":"search","collection":"vecs","vector":[1,0],"vector_name":"vec_l2","limit":2}`, driver.ErrSyntax},
		{`{"op":"count","collection":"nope"}`, driver.ErrNotFound},
	} {
		_, err := c.Query(ctx30(t), driver.Query{Text: bad.text})
		if !errors.Is(err, bad.kind) {
			t.Errorf("%s: err = %v, want %v", bad.text, err, bad.kind)
		}
	}
}

// TestBrowseSortedPaging pages People sorted by age across cursors: every
// point once, ages in order, NULL ages last.
func TestBrowseSortedPaging(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: peopleColl}
	for _, desc := range []bool{false, true} {
		var ages []driver.Value
		seen := map[int64]bool{}
		cursor := ""
		for page := 0; page < 10; page++ {
			rs, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Limit: 20, Cursor: cursor, Sort: []driver.SortKey{{Field: "age", Desc: desc}}})
			if err != nil {
				t.Fatal(err)
			}
			cols := rs.Columns()
			rows, st, err := driver.Collect(ctx30(t), rs, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				id, _ := r[colIndex(t, cols, "id")].AsInt()
				if seen[id] {
					t.Errorf("id %d twice", id)
				}
				seen[id] = true
				ages = append(ages, r[colIndex(t, cols, "age")])
			}
			if cursor = st.NextCursor; cursor == "" {
				break
			}
		}
		if len(seen) != len(drivertest.People.Rows) {
			t.Errorf("desc=%v: saw %d points", desc, len(seen))
		}
		nonNull := slices.IndexFunc(ages, driver.Value.IsNull)
		for i := 1; i < len(ages); i++ {
			if i >= nonNull && nonNull >= 0 {
				if !ages[i].IsNull() {
					t.Errorf("desc=%v: non-null age after the nulls at %d", desc, i)
				}
				continue
			}
			a, _ := ages[i-1].AsInt()
			b, _ := ages[i].AsInt()
			if (desc && b > a) || (!desc && b < a) {
				t.Errorf("desc=%v: age %d after %d", desc, b, a)
			}
		}
	}
	for _, s := range [][]driver.SortKey{{{Field: "name"}}, {{Field: "id", Desc: true}}, {{Field: "age"}, {Field: "name"}}} {
		_, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Sort: s})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("sort %v: err = %v, want ErrUnsupported", s, err)
		}
		// The pinned server's wording for a missing range index is what
		// sortError detects.
		if s[0].Field == "name" && len(s) == 1 && !strings.Contains(fmt.Sprint(err), "needs a range payload index") {
			t.Errorf("sort by an unindexed field: err = %v, want the range index hint", err)
		}
	}
	// Filters: a time range on the datetime field, a raw native filter.
	rs, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Limit: 100, Filter: driver.Filter{
		Conditions: []driver.Condition{{Field: "age", Op: driver.OpIsNull}},
		Raw:        `{"must":[{"field":{"key":"active","match":{"boolean":true}}}]}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := driver.Collect(ctx30(t), rs, 0)
	if len(rows) != 4 { // ids 11, 22, 44, 55 (33 is inactive)
		t.Errorf("null age and active: %d rows, want 4", len(rows))
	}
}

// TestDescribe checks the schema of People (declared and indexed types),
// of the vector collection and of the schemaless documents, which must
// also serialise to JSON for the AI tools.
func TestDescribe(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	sch, err := c.Describe(ctx30(t), driver.CollectionRef{Namespace: defaultNamespace, Name: peopleColl})
	if err != nil {
		t.Fatal(err)
	}
	if !sch.Schemaless || sch.RowEstimate != int64(len(drivertest.People.Rows)) || len(sch.Sample) != len(drivertest.People.Rows) {
		t.Errorf("people: schemaless %v rows %d sample %d", sch.Schemaless, sch.RowEstimate, len(sch.Sample))
	}
	var idx []string
	for _, i := range sch.Indexes {
		idx = append(idx, i.Name+":"+i.Method)
	}
	if !slices.Equal(idx, []string{"age:integer", "created_at:datetime"}) {
		t.Errorf("indexes = %v", idx)
	}
	vs, err := c.Describe(ctx30(t), vecsRef)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, f := range vs.Fields {
		types[f.Name] = f.Type.String() + " " + f.NativeType
	}
	if types["vec_cos"] != "vector vector(4, Cosine)" || types["label"] != "string string" {
		t.Errorf("vecs fields = %v", types)
	}
	ds, err := c.Describe(ctx30(t), driver.CollectionRef{Namespace: defaultNamespace, Name: docsColl})
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]driver.Field{}
	for _, f := range ds.Fields {
		fields[f.Name] = f
	}
	if f := fields["age"]; f.Type != driver.TypeUnknown || f.NativeType != "int|string" {
		t.Errorf("docs age = %+v", f)
	}
	if f := fields["address"]; f.Type != driver.TypeObject || len(f.Fields) != 3 {
		t.Errorf("docs address = %+v", f)
	}
	if _, err := json.Marshal(ds); err != nil {
		t.Errorf("describe output does not serialise: %v", err)
	}
	cols, rows, _ := collect(t, c, driver.Query{Text: `{"op":"scroll","collection":"docs"}`})
	if _, err := json.Marshal(map[string]any{"columns": cols, "rows": rows}); err != nil {
		t.Errorf("rows do not serialise: %v", err)
	}
}

// TestMutatorPoints: insert without an id gets a UUID, a taken id is
// ErrConflict, vectors are written and updated by column name.
func TestMutatorPoints(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	key, err := c.Insert(ctx, vecsRef, driver.Object{
		{Key: "label", Value: driver.String("new")},
		{Key: "vec_l2", Value: driver.Vector([]float32{9, 9, 9, 9})},
		{Key: "vec_cos", Value: driver.List(driver.Int(1), driver.Int(1), driver.Int(1), driver.Int(1))},
		{Key: "vec_ip", Value: driver.Vector([]float32{1, 2, 3, 4})},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := key.Get("id")
	if s, _ := id.AsText(); len(s) != 36 {
		t.Fatalf("generated id = %s", id)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, vecsRef, key) })
	if err := c.Update(ctx, vecsRef, key, driver.Object{{Key: "vec_l2", Value: driver.Vector([]float32{1, 0, 0, 0})}, {Key: "label", Value: driver.Null()}}); err != nil {
		t.Fatal(err)
	}
	_, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Field: "vec_l2", K: 1})
	if got := rows[0][0]; got.String() != id.String() {
		t.Errorf("after update the nearest is %s, want %s", got, id)
	}
	if _, err := c.Insert(ctx, vecsRef, driver.Object{{Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("taken id: err = %v, want ErrConflict", err)
	}
	if err := c.Update(ctx, vecsRef, key, driver.Object{{Key: "vec_l2", Value: driver.Vector([]float32{1})}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("short vector: err = %v, want ErrSyntax", err)
	}
	if err := c.Delete(ctx, vecsRef, driver.Object{{Key: "label", Value: driver.String("x")}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("key without id: err = %v, want ErrUnsupported", err)
	}
}

// TestBrowseCursorSortMismatch: a sorted cursor only fits its own sort.
func TestBrowseCursorSortMismatch(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: peopleColl}
	rs, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Limit: 10, Sort: []driver.SortKey{{Field: "age"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, st, _ := driver.Collect(ctx30(t), rs, 0)
	for _, s := range [][]driver.SortKey{{{Field: "age", Desc: true}}, {{Field: "created_at"}}, nil} {
		if _, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Limit: 10, Cursor: st.NextCursor, Sort: s}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("cursor of age asc with sort %v: err = %v, want ErrSyntax", s, err)
		}
	}
}

// TestInsertOnlyNeverReplaces pins the server behaviour Insert relies on:
// an insert-only upsert of a taken id leaves the stored point as it was
// (and reports no error, which is why Insert reads the payload back).
func TestInsertOnlyNeverReplaces(t *testing.T) {
	c, err := admin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	wait, mode := true, pb.UpdateMode_InsertOnly
	vec := pb.NewVectorDense([]float32{1, 1, 1, 1})
	_, err = c.GetPointsClient().Upsert(ctx30(t), &pb.UpsertPoints{CollectionName: vecsColl, Wait: &wait, UpdateMode: &mode,
		Points: []*pb.PointStruct{{Id: pb.NewIDNum(1), Payload: map[string]*pb.Value{"label": pb.NewValueString("PWNED")},
			Vectors: pb.NewVectorsMap(map[string]*pb.Vector{"vec_l2": vec, "vec_cos": vec, "vec_ip": vec})}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx30(t), &pb.GetPoints{CollectionName: vecsColl, Ids: []*pb.PointId{pb.NewIDNum(1)}, WithPayload: pb.NewWithPayload(true)})
	if err != nil || len(got) != 1 || got[0].GetPayload()["label"].GetStringValue() != "near" {
		t.Fatalf("point 1 after an insert-only upsert = %v, %v", got, err)
	}
}

// TestEditTypedFields: a field declared int keeps 64-bit integers exact
// through insert, read and update; a decimal-declared field stored as a
// number stays a number when edited; an update that changes a payload
// key and a vector writes both, and a refused vector changes nothing.
func TestEditTypedFields(t *testing.T) {
	coll := "tobira_typed"
	admin := createColl(t, coll, 2, pb.Distance_Euclid)
	types, _ := pb.NewStruct(map[string]any{"big": "int", "price": "decimal"})
	if err := admin.UpdateCollection(ctx30(t), &pb.UpdateCollection{CollectionName: coll,
		Metadata: map[string]*pb.Value{metadataKey: pb.NewValueFromFields(map[string]*pb.Value{"types": pb.NewValueStruct(types)})}}); err != nil {
		t.Fatal(err)
	}
	c := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: coll}
	const big = int64(1<<53 + 1)
	key, err := c.Insert(ctx, ref, driver.Object{
		{Key: "id", Value: driver.Int(1)}, {Key: "big", Value: driver.Int(big)},
		{Key: "vector", Value: driver.Vector([]float32{1, 2})},
	})
	if err != nil {
		t.Fatal(err)
	}
	wait := true
	if _, err := admin.SetPayload(ctx, &pb.SetPayloadPoints{CollectionName: coll, Wait: &wait, PointsSelector: pb.NewPointsSelector(pb.NewIDNum(1)),
		Payload: map[string]*pb.Value{"price": pb.NewValueDouble(1.5)}}); err != nil {
		t.Fatal(err)
	}
	read := func() (driver.Object, map[string]*pb.Value) {
		t.Helper()
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"op":"retrieve","collection":"tobira_typed","ids":[{"num":"1"}],"with_vectors":{"enable":true}}`})
		var o driver.Object
		for i, col := range cols {
			o = append(o, driver.Member{Key: col.Name, Value: rows[0][i]})
		}
		raw, err := admin.Get(ctx, &pb.GetPoints{CollectionName: coll, Ids: []*pb.PointId{pb.NewIDNum(1)}, WithPayload: pb.NewWithPayload(true)})
		if err != nil {
			t.Fatal(err)
		}
		return o, raw[0].GetPayload()
	}
	o, _ := read()
	if v, _ := o.Get("big"); func() bool { n, ok := v.AsInt(); return !ok || n != big }() {
		t.Errorf("big = %s, want %d exactly", v, big)
	}
	if err := c.Update(ctx, ref, key, driver.Object{
		{Key: "big", Value: driver.Int(math.MaxInt64)}, {Key: "price", Value: driver.Decimal("2.25")},
		{Key: "vector", Value: driver.Vector([]float32{3, 4})},
	}); err != nil {
		t.Fatal(err)
	}
	o, raw := read()
	if v, _ := o.Get("big"); func() bool { n, ok := v.AsInt(); return !ok || n != math.MaxInt64 }() {
		t.Errorf("big after update = %s", v)
	}
	if p := raw["price"]; p.GetDoubleValue() != 2.25 {
		t.Errorf("stored price = %v, want the number 2.25", p)
	}
	if v, _ := o.Get("price"); v.Type() != driver.TypeDecimal {
		t.Errorf("shown price = %s (%s), want a Decimal", v, v.Type())
	}
	if v, _ := o.Get("vector"); v.String() != driver.Vector([]float32{3, 4}).String() {
		t.Errorf("vector = %s", v)
	}
	err = c.Update(ctx, ref, key, driver.Object{{Key: "price", Value: driver.Decimal("9")}, {Key: "vector", Value: driver.Vector([]float32{1})}})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("short vector: err = %v", err)
	}
	if _, raw := read(); raw["price"].GetDoubleValue() != 2.25 {
		t.Errorf("a refused update changed price to %v", raw["price"])
	}
}

// TestQueryCaps: an oversized prefetch limit and offset are cut, with a
// warning each.
func TestQueryCaps(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	_, rows, st := collect(t, c, driver.Query{Text: `{"op":"query","collection":"vecs","using":"vec_l2","offset":"999999999",
		"prefetch":[{"query":{"nearest":{"dense":{"data":[1,0,0,0]}}},"using":"vec_l2","limit":"99999999"}],
		"query":{"nearest":{"dense":{"data":[1,0,0,0]}}},"limit":"3"}`})
	if len(rows) != 0 || len(st.Warnings) != 2 {
		t.Errorf("capped query: %d rows (offset beyond the data), warnings %q", len(rows), st.Warnings)
	}
}

// TestSimilarResolvesBeforeEmbedding: a missing collection or vector costs
// no embedder call.
func TestSimilarResolvesBeforeEmbedding(t *testing.T) {
	e := &stubEmbedder{vecs: [][]float32{{1, 0, 0, 0}}}
	c := open(t, driver.ConnConfig{Embedder: e})
	for _, tt := range []struct {
		ref   driver.CollectionRef
		field string
	}{{driver.CollectionRef{Namespace: defaultNamespace, Name: "nope"}, ""}, {vecsRef, "nope"}, {vecsRef, ""}} {
		if _, err := c.Similar(ctx30(t), tt.ref, driver.VectorQuery{Text: "x", Field: tt.field}); err == nil {
			t.Errorf("%v %q: want an error", tt.ref, tt.field)
		}
	}
	if n := e.calls.Load(); n != 0 {
		t.Errorf("embedder called %d times for searches that could not run", n)
	}
	_, rows, _ := similar(t, c, vecsRef, driver.VectorQuery{Text: "x", Field: "vec_l2", K: 1})
	if id, _ := rows[0][0].AsInt(); id != 1 || e.calls.Load() != 1 {
		t.Errorf("text search: top %d, %d embedder calls", id, e.calls.Load())
	}
}

// TestInsertLostRace stages another writer creating the id between
// Insert's check and its insert-only write. Qdrant then skips the write
// without an error; Insert must still report ErrConflict, also when it
// writes no payload, and when the other point's payload holds the written
// keys plus more. An insert without payload that wins leaves no marker.
func TestInsertLostRace(t *testing.T) {
	coll := "tobira_race"
	adm := createColl(t, coll, 2, pb.Distance_Cosine)
	c := open(t, driver.ConnConfig{})
	ctx := ctx30(t)
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: coll}
	wait := true
	race := func(id uint64, payload map[string]*pb.Value) func() {
		return func() {
			if _, err := adm.Upsert(ctx, &pb.UpsertPoints{CollectionName: coll, Wait: &wait, Points: []*pb.PointStruct{
				{Id: pb.NewIDNum(id), Payload: payload, Vectors: pb.NewVectorsDense([]float32{3, 4})}}}); err != nil {
				t.Error(err)
			}
		}
	}
	stored := func(id uint64) map[string]*pb.Value {
		t.Helper()
		got, err := adm.Get(ctx, &pb.GetPoints{CollectionName: coll, Ids: []*pb.PointId{pb.NewIDNum(id)}, WithPayload: pb.NewWithPayload(true)})
		if err != nil || len(got) != 1 {
			t.Fatalf("point %d: %v, %v", id, got, err)
		}
		return got[0].GetPayload()
	}

	c.beforeInsertWrite = race(1, nil)
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "vector", Value: driver.Vector([]float32{1, 0})}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("insert without payload that lost the race: err = %v, want ErrConflict", err)
	}
	if p := stored(1); len(p) != 0 {
		t.Errorf("the other writer's point changed: payload %v", p)
	}

	c.beforeInsertWrite = race(3, map[string]*pb.Value{"label": pb.NewValueString("a"), "extra": pb.NewValueString("PWNED")})
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(3)}, {Key: "label", Value: driver.String("a")},
		{Key: "vector", Value: driver.Vector([]float32{1, 0})}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("insert whose payload is a subset of the winner's: err = %v, want ErrConflict", err)
	}

	c.beforeInsertWrite = nil
	key, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(2)}, {Key: "vector", Value: driver.Vector([]float32{1, 0})}})
	if err != nil || len(key) != 1 {
		t.Fatalf("insert without payload: %v, %v", key, err)
	}
	if p := stored(2); len(p) != 0 {
		t.Errorf("insert without payload left payload %v, want none", p)
	}

	// The winner is another tobira insert, its marker still on the point:
	// the loser must not remove it.
	c.beforeInsertWrite = race(4, map[string]*pb.Value{insertMarkerKey: pb.NewValueString("someone-else")})
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(4)}, {Key: "vector", Value: driver.Vector([]float32{1, 0})}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("insert that lost to another tobira insert: err = %v, want ErrConflict", err)
	}
	if p := stored(4); len(p) != 1 || p[insertMarkerKey].GetStringValue() != "someone-else" {
		t.Errorf("the winner's marker changed: payload %v", p)
	}
	c.beforeInsertWrite = nil

	// A winning insert with payload leaves only its payload.
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(5)}, {Key: "label", Value: driver.String("b")},
		{Key: "vector", Value: driver.Vector([]float32{1, 0})}}); err != nil {
		t.Fatalf("insert with payload: %v", err)
	}
	if p := stored(5); len(p) != 1 || p["label"].GetStringValue() != "b" {
		t.Errorf("insert with payload left payload %v, want only label", p)
	}

	// Another client sets payload on the new point between the write and
	// the read-back: still our insert, not a conflict, and no marker left.
	c.afterInsertWrite = func() {
		if _, err := adm.SetPayload(ctx, &pb.SetPayloadPoints{CollectionName: coll, Wait: &wait,
			Payload: map[string]*pb.Value{"other": pb.NewValueString("x")}, PointsSelector: pb.NewPointsSelector(pb.NewIDNum(6))}); err != nil {
			t.Error(err)
		}
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(6)}, {Key: "label", Value: driver.String("c")},
		{Key: "vector", Value: driver.Vector([]float32{1, 0})}}); err != nil {
		t.Errorf("insert with a concurrent set_payload: err = %v, want nil", err)
	}
	if p := stored(6); len(p) != 2 || p["label"].GetStringValue() != "c" || p["other"].GetStringValue() != "x" {
		t.Errorf("after a concurrent set_payload: payload %v, want label and other only", p)
	}

	// The caller's ctx ends right after the write: the read-back fails, and
	// the marker is still removed.
	ictx, cancel := context.WithCancel(ctx)
	c.afterInsertWrite = func() { cancel() }
	if _, err := c.Insert(ictx, ref, driver.Object{{Key: "id", Value: driver.Int(7)}, {Key: "vector", Value: driver.Vector([]float32{1, 0})}}); err == nil {
		t.Error("insert whose ctx ended after the write: want an error")
	}
	c.afterInsertWrite = nil
	if p := stored(7); len(p) != 0 {
		t.Errorf("insert interrupted after the write left payload %v, want none", p)
	}
}
