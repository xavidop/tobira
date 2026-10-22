//go:build integration

package weaviate

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

var (
	peopleRef = driver.CollectionRef{Namespace: defaultNamespace, Name: peopleColl}
	vecsRef   = driver.CollectionRef{Namespace: defaultNamespace, Name: vecsColl}
	docsRef   = driver.CollectionRef{Namespace: defaultNamespace, Name: docsColl}
)

func TestIntegrationCatalog(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := context.Background()
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: defaultNamespace})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, col := range cols {
		names = append(names, col.Ref.Name)
	}
	if !slices.IsSorted(names) || !slices.Contains(names, peopleColl) || !slices.Contains(names, vecsColl) {
		t.Errorf("collections = %v", names)
	}
	s, err := c.Describe(ctx, peopleRef)
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	for _, f := range s.Fields {
		fields = append(fields, f.Name+":"+f.Type.String())
	}
	if got := strings.Join(fields, ","); got != "id:int,name:string,email:string,age:int,score:float,active:bool,created_at:time,tags:list,vector:vector" {
		t.Errorf("fields = %s", got)
	}
	if s.RowEstimate != int64(len(drivertest.People.Rows)) || !slices.Equal(s.PrimaryKey, []string{"id"}) || s.Comment == "" || s.DDL == "" {
		t.Errorf("schema = rows %d key %v comment %q ddl %d bytes", s.RowEstimate, s.PrimaryKey, s.Comment, len(s.DDL))
	}
	// A lower-case name finds the class too, under its canonical name.
	if s, err := c.Describe(ctx, driver.CollectionRef{Name: "tobiraPeople"}); err != nil || s.Ref.Name != peopleColl {
		t.Errorf("describe tobiraPeople = %v, %v", s, err)
	}
	d, err := c.Describe(ctx, docsRef)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]driver.Field{}
	for _, f := range d.Fields {
		byName[f.Name] = f
	}
	if f := byName["meta"]; f.Type != driver.TypeObject || len(f.Fields) != 2 || f.Fields[1].Name != "pages" || f.Fields[1].Type != driver.TypeInt {
		t.Errorf("meta = %+v", f)
	}
	if f := byName["where"]; f.Type != driver.TypeObject || f.NativeType != "geoCoordinates" {
		t.Errorf("where = %+v", f)
	}
	if f := byName["vector"]; f.Type != driver.TypeVector || f.NativeType != "vector(4)" {
		t.Errorf("vector = %+v", f)
	}
	v, err := c.Describe(ctx, vecsRef)
	if err != nil {
		t.Fatal(err)
	}
	var vecs []string
	for _, idx := range v.Indexes {
		vecs = append(vecs, idx.Name+"="+idx.Method)
	}
	if got := strings.Join(vecs, ","); !strings.Contains(got, "cos=flat cosine") || !strings.Contains(got, "l2=flat l2-squared") {
		t.Errorf("vector indexes = %s", got)
	}
	if _, err := c.Describe(ctx, driver.CollectionRef{Name: "Nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}

func TestIntegrationQuery(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	// Fields, where, sort, limit and offset.
	cols, rows, _ := query(t, c, driver.Query{Text: `{"collection":"TobiraPeople","fields":["name","age"],
		"where":{"path":["age"],"operator":"GreaterThan","valueInt":55},"sort":[{"path":["age"],"order":"desc"}],"limit":3,"offset":1}`})
	if colNames(cols) != "id,name,age" || len(rows) != 3 {
		t.Fatalf("cols %s, %d rows", colNames(cols), len(rows))
	}
	for i := 1; i < len(rows); i++ {
		a, _ := rows[i-1][2].AsInt()
		b, _ := rows[i][2].AsInt()
		if a < b || b <= 55 {
			t.Errorf("ages not descending above 55: %v", rows)
		}
	}
	// No limit with a where pages through every match.
	_, rows, _ = query(t, c, driver.Query{Text: `{"collection":"TobiraPeople","where":{"path":["active"],"operator":"Equal","valueBoolean":true}}`})
	if len(rows) != 38 {
		t.Errorf("active rows = %d, want 38", len(rows))
	}
	// Query.Limit truncates a stream.
	_, rows, st := query(t, c, driver.Query{Text: `{"collection":"TobiraPeople"}`, Limit: 5})
	if len(rows) != 5 || !st.Truncated {
		t.Errorf("limit 5: %d rows, truncated %v", len(rows), st.Truncated)
	}
	// after resumes the cursor.
	_, rows, _ = query(t, c, driver.Query{Text: `{"collection":"TobiraPeople","after":"00000000-0000-0000-0000-000000000037","fields":["name"]}`})
	if len(rows) != 2 || !drivertest.EqualValue(driver.Int(56), rows[0][0]) {
		t.Errorf("after 55: %v", rows)
	}
	// Small pages: the cursor stream and offset pages both cross pages.
	small := open(t, driver.ConnConfig{})
	small.pageSize = 10
	_, rows, _ = query(t, small, driver.Query{Text: `{"collection":"TobiraPeople"}`})
	for i, r := range rows {
		if !drivertest.EqualValue(driver.Int(int64(i+1)), r[0]) {
			t.Fatalf("stream row %d has id %s", i, r[0])
		}
	}
	if len(rows) != len(drivertest.People.Rows) {
		t.Errorf("stream over pages of 10: %d rows", len(rows))
	}
	_, rows, _ = query(t, small, driver.Query{Text: `{"collection":"TobiraPeople","where":{"path":["active"],"operator":"Equal","valueBoolean":true},"sort":[{"path":["active"]}]}`})
	if len(rows) != 38 {
		t.Errorf("offset pages of 10: %d rows, want 38", len(rows))
	}
	_, rows, st = query(t, small, driver.Query{Text: `{"collection":"TobiraPeople"}`, Limit: 25})
	if len(rows) != 25 || !st.Truncated {
		t.Errorf("limit 25 over pages of 10: %d rows, truncated %v", len(rows), st.Truncated)
	}
	// nearVector gives a _distance, bm25 a _score.
	cols, rows, _ = query(t, c, driver.Query{Text: `{"collection":"Vecs","nearVector":{"vector":[1,0,0,0],"targetVectors":["l2"]},"limit":2,"vectors":true}`})
	if colNames(cols) != "id,_distance,payload,cos,dot,l2" || len(rows) != 2 || !drivertest.EqualValue(driver.Int(1), rows[0][0]) {
		t.Errorf("nearVector: %s %v", colNames(cols), rows)
	}
	if v, ok := rows[0][5].AsVector(); !ok || len(v) != 4 {
		t.Errorf("l2 vector = %s", rows[0][5])
	}
	if p, ok := rows[0][2].AsObject(); !ok || len(p) != 2 || p[0].Key != "label" {
		t.Errorf("payload = %s", rows[0][2])
	}
	cols, rows, _ = query(t, c, driver.Query{Text: `{"collection":"Docs","bm25":{"query":"database","properties":["title","body"]},"fields":["title"]}`})
	if colNames(cols) != "id,_score,title" || len(rows) != 3 || !drivertest.EqualValue(driver.Int(1), rows[0][0]) {
		t.Errorf("bm25: %s %v", colNames(cols), rows)
	}
	if s, ok := rows[0][1].AsFloat(); !ok || s <= 0 {
		t.Errorf("_score = %s", rows[0][1])
	}
	// Every Weaviate type reads back typed.
	cols, rows, _ = query(t, c, driver.Query{Text: `{"collection":"Docs","limit":1}`})
	if colNames(cols) != "id,title,body,uid,where,ratings,meta" || len(rows) != 1 {
		t.Fatalf("docs: %s %v", colNames(cols), rows)
	}
	want := []driver.Type{driver.TypeInt, driver.TypeString, driver.TypeString, driver.TypeString, driver.TypeObject, driver.TypeList, driver.TypeObject}
	for i, w := range want {
		if rows[0][i].Type() != w {
			t.Errorf("docs column %s = %s (%s), want %s", cols[i].Name, rows[0][i], rows[0][i].Type(), w)
		}
	}
	// Raw GraphQL Get, with aliases and _additional.
	cols, rows, _ = query(t, c, driver.Query{Text: `{ Get { TobiraPeople(where: {path: ["id"], operator: Equal, valueText: "00000000-0000-0000-0000-000000000002"}) { who: name created_at _additional { id creationTimeUnix } } } }`})
	if colNames(cols) != "who,created_at,id,_creationTimeUnix" || len(rows) != 1 {
		t.Fatalf("raw get: %s %v", colNames(cols), rows)
	}
	if rows[0][1].Type() != driver.TypeTime || !drivertest.EqualValue(driver.Int(2), rows[0][2]) {
		t.Errorf("raw get row = %v", rows[0])
	}
	_, rows, st = query(t, c, driver.Query{Text: `{ Get { TobiraPeople(limit: 20) { name } } }`, Limit: 4})
	if len(rows) != 4 || !st.Truncated {
		t.Errorf("raw limit: %d rows, truncated %v", len(rows), st.Truncated)
	}
	// Raw Aggregate.
	cols, rows, _ = query(t, c, driver.Query{Text: `{ Aggregate { TobiraPeople { meta { count } age { maximum } } } }`})
	if colNames(cols) != "meta,age" || len(rows) != 1 {
		t.Fatalf("aggregate: %s %v", colNames(cols), rows)
	}
	if m, _ := rows[0][0].AsObject(); len(m) != 1 || !drivertest.EqualValue(driver.Int(57), m[0].Value) {
		t.Errorf("count = %s", rows[0][0])
	}
	// Errors.
	for _, tt := range []struct {
		text string
		kind error
	}{
		{`{"collection":"Nope"}`, driver.ErrNotFound},
		{`{ Get { Nope { a } } }`, driver.ErrNotFound},
		{`{"collection":"TobiraPeople","fields":["nope"]}`, driver.ErrNotFound},
		{`{ Get { TobiraPeople { nope } } }`, driver.ErrSyntax},
		{`{"collection":"TobiraPeople","where":{"path":["age"],"operator":"Equal","valueText":"x"}}`, driver.ErrSyntax},
	} {
		_, err := c.Query(context.Background(), driver.Query{Text: tt.text})
		if !errors.Is(err, tt.kind) {
			t.Errorf("Query(%s) = %v, want %v", tt.text, err, tt.kind)
		}
	}
}

func TestIntegrationReadOnly(t *testing.T) {
	ctx := context.Background()
	// A class whose vectorizer is a paid remote service. Creating it needs
	// no key; querying it with nearText would call OpenAI.
	if err := mustREST(ctx, http.MethodPost, "/schema", map[string]any{
		"class": "RemoteDocs", "vectorizer": "text2vec-openai",
		"properties": []map[string]any{{"name": "title", "dataType": []string{"text"}}},
	}); err != nil {
		t.Fatalf("create RemoteDocs (is text2vec-openai enabled in the image?): %v", err)
	}
	t.Cleanup(func() { _, _, _ = rest(context.Background(), http.MethodDelete, "/schema/RemoteDocs", nil) })
	ro := open(t, driver.ConnConfig{ReadOnly: true})
	rw := open(t, driver.ConnConfig{})
	refused := []string{
		`{"collection":"RemoteDocs","nearText":{"concepts":["x"]}}`,
		`{"collection":"RemoteDocs","hybrid":{"query":"x"}}`,
		`{ Get { RemoteDocs(nearText: {concepts: ["x"]}) { title } } }`,
		`{ Get { RemoteDocs(hybrid: {query: "x"}) { title } } }`,
		`{ Aggregate { RemoteDocs(nearText: {concepts: ["x"]}, objectLimit: 1) { meta { count } } } }`,
		`{ Get { TobiraPeople { name _additional { generate(singleResult: {prompt: "x {name}"}) { singleResult } } } } }`,
		`{ Get { TobiraPeople(ask: {question: "x"}) { name } } }`,
		`{ Get { TobiraPeople { name _additional { rerank(property: "name", query: "x") { score } } } } }`,
		peopleWrite,
	}
	for _, text := range refused {
		if _, err := ro.Query(ctx, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("RO conn: Query(%s) = %v, want ErrReadOnly", text, err)
		}
		if _, err := rw.Query(ctx, driver.Query{Text: text, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Query.ReadOnly: Query(%s) = %v, want ErrReadOnly", text, err)
		}
	}
	allowed := []string{
		`{"collection":"Docs","bm25":{"query":"database"}}`,
		`{"collection":"Vecs","nearVector":{"vector":[1,0,0,0],"targetVectors":["cos"]},"limit":1}`,
		`{"collection":"Vecs","nearObject":{"id":"00000000-0000-0000-0000-000000000001","targetVectors":["cos"]},"limit":1}`,
		`{ Get { TobiraPeople(limit: 2) { name _additional { id lastUpdateTimeUnix } } } }`,
		`{ Aggregate { TobiraPeople(groupBy: ["active"]) { meta { count } groupedBy { value } } } }`,
		`{"collection":"RemoteDocs","limit":1}`,
	}
	for _, text := range allowed {
		_, _, _ = query(t, ro, driver.Query{Text: text})
	}
	// Browse, Similar and the catalog work on a read-only connection.
	if _, err := ro.Browse(ctx, peopleRef, driver.BrowseOptions{Limit: 5}); err != nil {
		t.Errorf("RO browse: %v", err)
	}
	if _, err := ro.Similar(ctx, vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Field: "cos"}); err != nil {
		t.Errorf("RO similar: %v", err)
	}
}

func TestIntegrationBrowse(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := context.Background()
	page := func(opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats) {
		t.Helper()
		rs, err := c.Browse(ctx, peopleRef, opts)
		if err != nil {
			t.Fatalf("Browse(%+v): %v", opts, err)
		}
		rows, st, err := driver.Collect(ctx, rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		return rs.Columns(), rows, st
	}
	cond := func(f string, op driver.Op, v driver.Value) driver.BrowseOptions {
		return driver.BrowseOptions{Limit: 100, Filter: driver.Filter{Conditions: []driver.Condition{{Field: f, Op: op, Value: v}}}}
	}
	for _, tt := range []struct {
		opts driver.BrowseOptions
		want int
	}{
		{cond("name", driver.OpContains, driver.String("ing")), 4}, // a token of "Alan Turing"
		{cond("name", driver.OpPrefix, driver.String("Ada")), 4},   // "Ada Lovelace"
		{cond("age", driver.OpIsNull, driver.Null()), 5},           // 11, 22, 33, 44, 55
		{cond("age", driver.OpNotNull, driver.Null()), 52},
		{cond("tags", driver.OpContains, driver.String("admin")), 19}, // ["admin","beta"] and ["admin"]
		{cond("id", driver.OpIn, driver.List(driver.Int(3), driver.Int(4), driver.Int(99))), 2},
		{cond("id", driver.OpIn, driver.List()), 0},
		{driver.BrowseOptions{Limit: 100, Filter: driver.Filter{Raw: `{"path":["age"],"operator":"LessThan","valueInt":22}`}}, 3}, // ids 1, 45, 46
	} {
		_, rows, _ := page(tt.opts)
		if tt.want >= 0 && len(rows) != tt.want {
			t.Errorf("Browse(%+v): %d rows, want %d", tt.opts.Filter, len(rows), tt.want)
		}
	}
	// Sorted pages follow each other without overlap.
	seen := map[int64]bool{}
	cursor := ""
	for range 10 {
		cols, rows, st := page(driver.BrowseOptions{Limit: 25, Sort: []driver.SortKey{{Field: "score", Desc: true}}, Cursor: cursor})
		id := col(t, cols, "id")
		for _, r := range rows {
			n, _ := r[id].AsInt()
			if seen[n] {
				t.Errorf("id %d twice", n)
			}
			seen[n] = true
		}
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if len(seen) != len(drivertest.People.Rows) {
		t.Errorf("sorted paging saw %d ids", len(seen))
	}
	// A filtered browse pages too.
	_, rows, st := page(driver.BrowseOptions{Limit: 30, Filter: driver.Filter{Conditions: []driver.Condition{{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)}}}})
	if len(rows) != 30 || st.NextCursor == "" {
		t.Errorf("filtered page 1: %d rows, cursor %q", len(rows), st.NextCursor)
	}
	for _, bad := range []driver.BrowseOptions{
		{Sort: []driver.SortKey{{Field: "vector"}}},
		{Cursor: "zz"},
		{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "nope", Op: driver.OpEq, Value: driver.Int(1)}}}},
	} {
		if _, err := c.Browse(ctx, peopleRef, bad); err == nil {
			t.Errorf("Browse(%+v) succeeded", bad)
		}
	}
	if _, err := c.Browse(ctx, driver.CollectionRef{Name: "Nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing collection: %v", err)
	}
}

func TestIntegrationMutate(t *testing.T) {
	c := open(t, driver.ConnConfig{})
	ctx := context.Background()
	// Insert without an id: Weaviate assigns a UUID.
	key, err := c.Insert(ctx, docsRef, driver.Object{{Key: "title", Value: driver.String("fresh")}, {Key: "vector", Value: driver.Vector([]float32{0, 0, 1, 0})}})
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := key.Get("id"); id.Type() != driver.TypeString {
		t.Errorf("generated key = %v", key)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), docsRef, key) })
	// An id in use is a conflict.
	if _, err := c.Insert(ctx, docsRef, driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "title", Value: driver.String("dup")}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("duplicate insert = %v", err)
	}
	// Unknown properties are refused rather than created by auto-schema.
	if _, err := c.Insert(ctx, docsRef, driver.Object{{Key: "brand_new", Value: driver.String("x")}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown property = %v", err)
	}
	// Update a nested object, a list, a vector, and clear a value with NULL.
	changes := driver.Object{
		{Key: "meta", Value: driver.ObjectValue(driver.Member{Key: "lang", Value: driver.String("nl")}, driver.Member{Key: "pages", Value: driver.Int(3)})},
		{Key: "ratings", Value: driver.List(driver.Float(4.5))},
		{Key: "body", Value: driver.Null()},
		{Key: "vector", Value: driver.List(driver.Float(0), driver.Float(1), driver.Float(0), driver.Float(0))},
	}
	if err := c.Update(ctx, docsRef, key, changes); err != nil {
		t.Fatal(err)
	}
	id, _ := key.Get("id")
	uuid, _ := id.AsText()
	cols, rows, _ := query(t, c, driver.Query{Text: `{"collection":"Docs","vectors":true,"where":{"path":["id"],"operator":"Equal","valueText":"` + uuid + `"}}`})
	if len(rows) != 1 {
		t.Fatalf("read back: %v", rows)
	}
	r := rows[0]
	if v := r[col(t, cols, "title")]; !drivertest.EqualValue(driver.String("fresh"), v) {
		t.Errorf("title = %s", v)
	}
	if v := r[col(t, cols, "body")]; !v.IsNull() {
		t.Errorf("body = %s, want NULL", v)
	}
	if v := r[col(t, cols, "meta")]; !drivertest.EqualValue(changes[0].Value, v) {
		t.Errorf("meta = %s", v)
	}
	if v, _ := r[col(t, cols, "vector")].AsVector(); len(v) != 4 || v[1] != 1 {
		t.Errorf("vector = %v", v)
	}
	for _, tt := range []struct {
		name string
		err  error
		kind error
	}{
		{"update missing", c.Update(ctx, docsRef, driver.Object{{Key: "id", Value: driver.Int(999)}}, driver.Object{{Key: "title", Value: driver.String("x")}}), driver.ErrNotFound},
		{"delete missing", c.Delete(ctx, docsRef, driver.Object{{Key: "id", Value: driver.Int(999)}}), driver.ErrNotFound},
		{"change id", c.Update(ctx, docsRef, key, driver.Object{{Key: "id", Value: driver.Int(5)}}), driver.ErrUnsupported},
		{"bad type", c.Update(ctx, docsRef, key, driver.Object{{Key: "title", Value: driver.Int(5)}}), driver.ErrSyntax},
		{"bad vector size", c.Update(ctx, docsRef, key, driver.Object{{Key: "vector", Value: driver.Vector([]float32{1})}}), driver.ErrSyntax},
	} {
		if !errors.Is(tt.err, tt.kind) {
			t.Errorf("%s = %v, want %v", tt.name, tt.err, tt.kind)
		}
	}
	// Named vectors are written by their column.
	vk := driver.Object{{Key: "id", Value: driver.Int(100)}}
	if _, err := c.Insert(ctx, vecsRef, driver.Object{vk[0], {Key: "l2", Value: driver.Vector([]float32{9, 9, 9, 9})}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, vecsRef, vk); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationSimilar(t *testing.T) {
	ctx := context.Background()
	c := open(t, driver.ConnConfig{Embedder: drivertest.HashEmbedder{Dims: 4}})
	rs, err := c.Similar(ctx, vecsRef, driver.VectorQuery{Text: "near", Field: "cos", K: 6})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) != 6 {
		t.Fatalf("text similar: %d rows, %v", len(rows), err)
	}
	// A filter narrows the neighbours.
	rs, err = c.Similar(ctx, vecsRef, driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Metric: driver.MetricL2, K: 10,
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpNe, Value: driver.Int(1)}}}})
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	rows, _, _ = driver.Collect(ctx, rs, 0)
	if len(rows) != 5 || !drivertest.EqualValue(driver.Int(2), rows[0][0]) && !drivertest.EqualValue(driver.Int(4), rows[0][0]) {
		t.Errorf("filtered: %v", rows)
	}
	// _distance is the true L2 distance: |(1,0,0,0) - (0,1,0,0)| = sqrt 2.
	for _, r := range rows {
		if drivertest.EqualValue(driver.Int(4), r[0]) {
			d, _ := r[col(t, cols, "_distance")].AsFloat()
			if d < 1.414 || d > 1.415 {
				t.Errorf("distance to id 4 = %v, want sqrt 2", d)
			}
		}
	}
	for _, tt := range []struct {
		q    driver.VectorQuery
		kind error
	}{
		{driver.VectorQuery{Vector: []float32{1, 0}, Field: "cos"}, driver.ErrSyntax},
		{driver.VectorQuery{Vector: []float32{1, 0, 0, 0}}, driver.ErrSyntax},
		{driver.VectorQuery{Vector: []float32{1, 0, 0, 0}, Field: "cos", Metric: driver.MetricL2}, driver.ErrUnsupported},
	} {
		if _, err := c.Similar(ctx, vecsRef, tt.q); !errors.Is(err, tt.kind) {
			t.Errorf("Similar(%+v) = %v, want %v", tt.q, err, tt.kind)
		}
	}
	plain := open(t, driver.ConnConfig{})
	if _, err := plain.Similar(ctx, vecsRef, driver.VectorQuery{Text: "x", Field: "cos"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("no embedder = %v", err)
	}
}

func TestIntegrationAPIKey(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []driver.ConnConfig{
		{DSN: "weaviate://" + testAuthHost, Password: driver.NewSecret(testAPIKey)},
		{DSN: "weaviate://" + testAuthHost + "?api_key=" + testAPIKey},
		{DSN: "weaviate://:" + testAPIKey + "@" + testAuthHost},
	} {
		c, err := New().Open(ctx, cfg)
		if err != nil {
			t.Errorf("Open(%s) with the key: %v", driver.RedactDSN(cfg.DSN), err)
			continue
		}
		if _, err := c.Collections(ctx, driver.NamespaceRef{}); err != nil {
			t.Errorf("collections: %v", err)
		}
		_ = c.Close()
	}
	for _, cfg := range []driver.ConnConfig{
		{DSN: "weaviate://" + testAuthHost},
		{DSN: "weaviate://" + testAuthHost + "?apikey=wrong-key-1234"},
		{DSN: "weaviate://" + testAuthHost, Password: driver.NewSecret("wrong-key-1234")},
	} {
		c, err := New().Open(ctx, cfg)
		if err == nil {
			_ = c.Close()
			t.Errorf("Open(%s) succeeded", driver.RedactDSN(cfg.DSN))
			continue
		}
		if !errors.Is(err, driver.ErrAuth) {
			t.Errorf("Open(%s) = %v, want ErrAuth", driver.RedactDSN(cfg.DSN), err)
		}
		if strings.Contains(err.Error(), "wrong-key-1234") {
			t.Errorf("the error carries the key: %v", err)
		}
	}
}
