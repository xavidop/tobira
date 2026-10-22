//go:build integration

package firestore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	fs "cloud.google.com/go/firestore"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// collect runs a query and drains it.
func collect(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(ctxT(t), q)
	if err != nil {
		t.Fatalf("Query(%s): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatalf("Query(%s): %v", q.Text, err)
	}
	return cols, rows, st
}

func col(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no column %q in %+v", name, cols)
	}
	return i
}

func ids(t *testing.T, cols []driver.Column, rows []driver.Row) []string {
	t.Helper()
	i := col(t, cols, "_id")
	out := make([]string, len(rows))
	for k, r := range rows {
		out[k], _ = r[i].AsText()
	}
	return out
}

func TestQueryForms(t *testing.T) {
	c := openTest(t, testProject, false)
	t.Run("WhereOrderLimitSelect", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"tobira_people","where":[["active","==",true],["id","<=",10]],` +
			`"orderBy":[["id","desc"]],"limit":3,"select":["name","id"]}`})
		if got := ids(t, cols, rows); !slices.Equal(got, []string{"10", "8", "7"}) {
			t.Errorf("ids = %v", got)
		}
		if len(cols) != 3 || cols[0].Name != "_id" || !cols[0].Key || cols[0].Source == nil || cols[0].Source.Name != "tobira_people" {
			t.Errorf("cols = %+v", cols)
		}
	})
	t.Run("DocumentID", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"tobira_people","where":[["_id","in",["3","4"]]]}`})
		if got := ids(t, cols, rows); !slices.Equal(got, []string{"3", "4"}) {
			t.Errorf("ids = %v", got)
		}
	})
	t.Run("Timestamp", func(t *testing.T) {
		_, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"tobira_people","where":[["created_at","<",{"$timestamp":"2026-01-04T00:00:00Z"}]]}`})
		if len(rows) != 2 { // 2026-01-02T03:04:05Z and +26h
			t.Errorf("got %d rows, want 2", len(rows))
		}
	})
	t.Run("ArrayContains", func(t *testing.T) {
		_, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"tobira_people","where":[["tags","array-contains","admin"]]}`})
		want := 0
		for _, r := range drivertest.People.Rows {
			if raw, _ := r[7].AsJSON(); strings.Contains(string(raw), `"admin"`) {
				want++
			}
		}
		if len(rows) != want {
			t.Errorf("got %d rows, want %d", len(rows), want)
		}
	})
	t.Run("Document", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"document":"users/ada/orders/o2"}`})
		if len(rows) != 1 || cols[0].Source == nil || cols[0].Source.Name != "users/ada/orders" {
			t.Fatalf("rows %v cols %+v", rows, cols)
		}
		if n, _ := rows[0][col(t, cols, "total")].AsInt(); n != 20 {
			t.Errorf("total = %v", rows[0])
		}
	})
	t.Run("Subcollection", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"users/grace/orders","orderBy":[["total","desc"]]}`})
		if got := ids(t, cols, rows); !slices.Equal(got, []string{"o3", "o2", "o1"}) {
			t.Errorf("ids = %v", got)
		}
	})
	t.Run("CollectionGroup", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"collectionGroup":"orders","where":[["total",">=",20]]}`})
		if len(rows) != 4 || cols[1].Name != "_path" || cols[0].Source != nil {
			t.Fatalf("rows %d cols %+v", len(rows), cols)
		}
		p, _ := rows[0][1].AsText()
		if !strings.HasPrefix(p, "users/") || !strings.Contains(p, "/orders/") {
			t.Errorf("_path = %q", p)
		}
		_, rows, _ = collect(t, c, driver.Query{Text: `{"collectionGroup":"orders","where":[["_id","==","users/ada/orders/o1"]]}`})
		if len(rows) != 1 {
			t.Errorf("collection group by path: %d rows", len(rows))
		}
	})
	t.Run("Count", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"tobira_people","count":true,"where":[["active","==",false]]}`})
		if len(rows) != 1 || cols[0].Name != "count" {
			t.Fatalf("count result %v %+v", rows, cols)
		}
		if n, _ := rows[0][0].AsInt(); n != 19 { // ids divisible by 3
			t.Errorf("count = %d, want 19", n)
		}
	})
	t.Run("Reference", func(t *testing.T) {
		_, rows, _ := collect(t, c, driver.Query{Text: `{"collection":"typed","where":[["ref","==",{"$ref":"users/ada"}]]}`})
		if len(rows) != 1 {
			t.Errorf("ref filter: %d rows", len(rows))
		}
	})
	t.Run("LimitTruncates", func(t *testing.T) {
		_, rows, st := collect(t, c, driver.Query{Text: `{"collection":"tobira_people","limit":30}`, Limit: 10})
		if len(rows) != 10 || !st.Truncated {
			t.Errorf("rows %d truncated %v", len(rows), st.Truncated)
		}
		_, rows, st = collect(t, c, driver.Query{Text: `{"collection":"tobira_people","limit":5}`, Limit: 10})
		if len(rows) != 5 || st.Truncated {
			t.Errorf("user limit: rows %d truncated %v", len(rows), st.Truncated)
		}
	})
	t.Run("LateFieldsWarn", func(t *testing.T) {
		cols, rows, st := collect(t, c, driver.Query{Text: `{"collection":"tobira_big","orderBy":["n"]}`})
		if len(rows) != 1500 || slices.ContainsFunc(cols, func(c driver.Column) bool { return c.Name == "late" }) {
			t.Errorf("rows %d cols %+v", len(rows), cols)
		}
		if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "500 fields") {
			t.Errorf("warnings = %v", st.Warnings)
		}
	})
	t.Run("Errors", func(t *testing.T) {
		for _, tt := range []struct {
			q    driver.Query
			want error
		}{
			{driver.Query{Text: `{"document":"users/nobody"}`}, driver.ErrNotFound},
			{driver.Query{Text: `{"collection":"users"}`, Namespace: "other-db"}, driver.ErrNotFound},
			{driver.Query{Text: `{"collection":"users"}`, Params: []driver.Value{driver.Int(1)}}, driver.ErrUnsupported},
			{driver.Query{Text: `{"collection":"users","where":[["_id","==",5]]}`}, driver.ErrSyntax},
			{driver.Query{Text: `{"delete":"users/ada"}`}, driver.ErrUnsupported},
		} {
			_, err := c.Query(ctxT(t), tt.q)
			if !errors.Is(err, tt.want) {
				t.Errorf("Query(%s): want %v, got %v", tt.q.Text, tt.want, err)
			}
		}
	})
}

func TestTypes(t *testing.T) {
	c := openTest(t, testProject, false)
	cols, rows, _ := collect(t, c, driver.Query{Text: `{"document":"typed/t1"}`})
	r := rows[0]
	get := func(name string) (driver.Value, driver.Column) {
		i := col(t, cols, name)
		return r[i], cols[i]
	}
	if v, cl := get("ref"); v.String() != "users/ada" || cl.NativeType != "reference" {
		t.Errorf("ref = %s (%s)", v, cl.NativeType)
	}
	if v, cl := get("geo"); v.Type() != driver.TypeObject || cl.NativeType != "geopoint" {
		t.Errorf("geo = %s (%s)", v, cl.NativeType)
	} else if o, _ := v.AsObject(); len(o) != 2 || o[0].Key != "latitude" {
		t.Errorf("geo = %v", o)
	}
	if v, _ := get("bytes"); v.Type() != driver.TypeBytes {
		t.Errorf("bytes = %s", v.Type())
	}
	if v, _ := get("when"); v.Type() != driver.TypeTime {
		t.Errorf("when = %s", v.Type())
	} else if tm, _ := v.AsTime(); tm.Nanosecond() != 123456000 {
		t.Errorf("when lost microseconds: %v", tm)
	}
	if v, cl := get("vec"); v.Type() != driver.TypeVector || cl.NativeType != "vector" {
		t.Errorf("vec = %s (%s)", v, cl.NativeType)
	}
	if v, _ := get("nan"); v.Type() != driver.TypeFloat {
		t.Errorf("nan = %s", v.Type())
	}
	sch, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "typed"})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(sch.Fields, func(f driver.Field) bool { return f.Name == "mixed" })
	if i < 0 || sch.Fields[i].Type != driver.TypeFloat || sch.Fields[i].NativeType != "double|integer" {
		t.Errorf("mixed = %+v", sch.Fields)
	}
}

func TestCatalog(t *testing.T) {
	c := openTest(t, testProject, false)
	nss, err := c.Namespaces(ctxT(t))
	if err != nil || len(nss) != 1 || nss[0].Name != "(default)" || !nss[0].Default {
		t.Fatalf("namespaces = %+v, %v", nss, err)
	}
	cols, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "(default)"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, cl := range cols {
		names = append(names, cl.Ref.Name)
	}
	if !slices.Equal(names, []string{"docs", "tobira_big", "tobira_people", "typed", "users"}) {
		t.Errorf("collections = %v", names)
	}
	if _, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("other database: %v", err)
	}
	sch, err := c.Describe(ctxT(t), driver.CollectionRef{Name: drivertest.DocumentsCollection})
	if err != nil {
		t.Fatal(err)
	}
	if !sch.Schemaless || len(sch.Sample) != 5 || !slices.Equal(sch.PrimaryKey, []string{"_id"}) {
		t.Errorf("schema = %+v", sch)
	}
	field := func(fs []driver.Field, name string) *driver.Field {
		for i := range fs {
			if fs[i].Name == name {
				return &fs[i]
			}
		}
		return nil
	}
	if a := field(sch.Fields, "age"); a == nil || a.Type != driver.TypeUnknown || a.NativeType != "integer|string" || !a.Nullable {
		t.Errorf("age = %+v", a)
	}
	if a := field(sch.Fields, "address"); a == nil || field(a.Fields, "geo") == nil || field(field(a.Fields, "geo").Fields, "lat") == nil {
		t.Errorf("address = %+v", a)
	}
	sub, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "users/ada/orders"})
	if err != nil {
		t.Fatal(err)
	}
	if it := field(sub.Fields, "item"); it == nil || field(it.Fields, "sku") == nil {
		t.Errorf("subcollection fields = %+v", sub.Fields)
	}
	for _, name := range []string{"nope", "users/nobody/orders"} {
		if _, err := c.Describe(ctxT(t), driver.CollectionRef{Name: name}); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("Describe(%s): %v", name, err)
		}
	}
	if _, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "users/ada"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Describe(document path): %v", err)
	}
}

func TestBrowse(t *testing.T) {
	c := openTest(t, testProject, false)
	b, _ := driver.As[driver.Browser](c)
	ref := driver.CollectionRef{Name: "tobira_people"}
	page := func(opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats, error) {
		rs, err := b.Browse(ctxT(t), ref, opts)
		if err != nil {
			return nil, nil, driver.Stats{}, err
		}
		cols := rs.Columns()
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		return cols, rows, st, err
	}
	t.Run("Prefix", func(t *testing.T) {
		cols, rows, _, err := page(driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "email", Op: driver.OpPrefix, Value: driver.String("user1")}}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 10 { // user10..user19
			t.Errorf("prefix: %d rows (%v)", len(rows), ids(t, cols, rows))
		}
	})
	t.Run("Raw", func(t *testing.T) {
		_, rows, _, err := page(driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: `[["age","==",null]]`}})
		if err != nil || len(rows) != 5 {
			t.Errorf("raw: %d rows, %v", len(rows), err)
		}
	})
	t.Run("Contains", func(t *testing.T) {
		_, _, _, err := page(driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "name", Op: driver.OpContains, Value: driver.String("a")}}}})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("contains: %v", err)
		}
	})
	t.Run("CursorForOtherSort", func(t *testing.T) {
		_, _, st, err := page(driver.BrowseOptions{Limit: 5})
		if err != nil || st.NextCursor == "" {
			t.Fatalf("first page: %v", err)
		}
		if _, _, _, err := page(driver.BrowseOptions{Limit: 5, Cursor: st.NextCursor, Sort: []driver.SortKey{{Field: "age"}}}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("cursor with another sort: %v", err)
		}
	})
	t.Run("DeletedCursorDocument", func(t *testing.T) {
		a := admin(ctxT(t), t, testProject)
		coll := a.Collection("tobira_scratch")
		for _, id := range []string{"a", "b", "c"} {
			if _, err := coll.Doc(id).Set(ctxT(t), map[string]any{"x": id}); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			for _, id := range []string{"a", "b", "c"} {
				_, _ = coll.Doc(id).Delete(context.Background())
			}
		})
		rs, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_scratch"}, driver.BrowseOptions{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		_, st, _ := driver.Collect(ctxT(t), rs, 0)
		if _, err := coll.Doc("a").Delete(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_scratch"}, driver.BrowseOptions{Limit: 1, Cursor: st.NextCursor}); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("deleted cursor document: %v", err)
		}
	})
	t.Run("SubcollectionByID", func(t *testing.T) {
		rs, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "users/ada/orders"}, driver.BrowseOptions{
			Filter: driver.Filter{Conditions: []driver.Condition{{Field: "_id", Op: driver.OpNe, Value: driver.String("o2")}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		cols := rs.Columns()
		rows, _, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(t, cols, rows); !slices.Equal(got, []string{"o1", "o3"}) {
			t.Errorf("ids = %v", got)
		}
	})
	t.Run("SortByIDDesc", func(t *testing.T) {
		rs, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "users/ada/orders"}, driver.BrowseOptions{
			Limit: 2, Sort: []driver.SortKey{{Field: "_id", Desc: true}},
		})
		if errors.Is(err, driver.ErrUnsupported) && strings.Contains(err.Error(), "descending key scans") {
			t.Skip("the emulator cannot order by document id descending (Google's service can)")
		}
		if err != nil {
			t.Fatal(err)
		}
		cols := rs.Columns()
		rows, st, err := driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		got := ids(t, cols, rows)
		rs, err = b.Browse(ctxT(t), driver.CollectionRef{Name: "users/ada/orders"}, driver.BrowseOptions{
			Limit: 2, Cursor: st.NextCursor, Sort: []driver.SortKey{{Field: "_id", Desc: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		rows, _, err = driver.Collect(ctxT(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ids(t, rs.Columns(), rows)...)
		if !slices.Equal(got, []string{"o3", "o2", "o1"}) {
			t.Errorf("ids = %v", got)
		}
	})
}

func TestMutator(t *testing.T) {
	c := openTest(t, testProject, false)
	m, _ := driver.As[driver.Mutator](c)
	a := admin(ctxT(t), t, testProject)
	ref := driver.CollectionRef{Name: "users/ada/notes"}
	t.Cleanup(func() {
		docs, _ := a.Collection("users/ada/notes").Documents(context.Background()).GetAll()
		for _, d := range docs {
			_, _ = d.Ref.Delete(context.Background())
		}
	})
	key, err := m.Insert(ctxT(t), ref, driver.Object{{Key: "_id", Value: driver.String("n1")}, {Key: "text", Value: driver.String("hi")},
		{Key: "when", Value: driver.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}, {Key: "score", Value: driver.Decimal("2.50")}})
	if err != nil || len(key) != 1 || key[0].Value.String() != "n1" {
		t.Fatalf("Insert = %v, %v", key, err)
	}
	if _, err := m.Insert(ctxT(t), ref, driver.Object{{Key: "_id", Value: driver.String("n1")}}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("duplicate id: %v", err)
	}
	auto, err := m.Insert(ctxT(t), ref, driver.Object{{Key: "text", Value: driver.String("hi")}})
	if err != nil || len(auto[0].Value.String()) < 10 {
		t.Fatalf("auto id = %v, %v", auto, err)
	}
	snap, err := a.Doc("users/ada/notes/n1").Get(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := snap.DataAt("score"); v != 2.5 {
		t.Errorf("score stored as %#v", v)
	}
	if err := m.Update(ctxT(t), ref, key, driver.Object{{Key: "text", Value: driver.String("bye")}, {Key: "a.b", Value: driver.Null()}}); err != nil {
		t.Fatal(err)
	}
	snap, _ = a.Doc("users/ada/notes/n1").Get(ctxT(t))
	if v, _ := snap.DataAt("text"); v != "bye" {
		t.Errorf("text = %v", v)
	}
	if v, err := snap.DataAtPath(fs.FieldPath{"a.b"}); err != nil || v != nil {
		t.Errorf("a field named a.b: %v, %v", v, err)
	}
	// Two documents hold text "hi"? No: n1 is now "bye", the auto one "hi".
	if err := m.Update(ctxT(t), ref, driver.Object{{Key: "text", Value: driver.String("hi")}}, driver.Object{{Key: "x", Value: driver.Int(1)}}); err != nil {
		t.Errorf("update by a field key: %v", err)
	}
	if _, err := m.Insert(ctxT(t), ref, driver.Object{{Key: "text", Value: driver.String("hi")}}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"ambiguous key", m.Update(ctxT(t), ref, driver.Object{{Key: "text", Value: driver.String("hi")}}, driver.Object{{Key: "x", Value: driver.Int(2)}}), driver.ErrConflict},
		{"missing id", m.Update(ctxT(t), ref, driver.Object{{Key: "_id", Value: driver.String("nope")}}, driver.Object{{Key: "x", Value: driver.Int(2)}}), driver.ErrNotFound},
		{"missing field key", m.Delete(ctxT(t), ref, driver.Object{{Key: "text", Value: driver.String("never")}}), driver.ErrNotFound},
		{"change _id", m.Update(ctxT(t), ref, key, driver.Object{{Key: "_id", Value: driver.String("n2")}}), driver.ErrUnsupported},
		{"delete missing id", m.Delete(ctxT(t), ref, driver.Object{{Key: "_id", Value: driver.String("nope")}}), driver.ErrNotFound},
		{"bad path", m.Delete(ctxT(t), driver.CollectionRef{Name: "users/ada"}, key), driver.ErrSyntax},
	} {
		if !errors.Is(tt.err, tt.want) {
			t.Errorf("%s: want %v, got %v", tt.name, tt.want, tt.err)
		}
	}
	if err := m.Delete(ctxT(t), ref, key); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Doc("users/ada/notes/n1").Get(ctxT(t)); err == nil {
		t.Error("n1 still exists after Delete")
	}
}

func TestWatch(t *testing.T) {
	c := openTest(t, testProject, false)
	w, _ := driver.As[driver.Watcher](c)
	a := admin(ctxT(t), t, testProject)
	coll := a.Collection("tobira_watch")
	if _, err := coll.Doc("seed").Set(ctxT(t), map[string]any{"v": int64(0)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = coll.Doc("seed").Delete(context.Background())
		_, _ = coll.Doc("w1").Delete(context.Background())
	})
	for _, req := range []driver.WatchRequest{
		{Collection: &driver.CollectionRef{Name: "tobira_watch"}},
		{Channel: "tobira_watch/w1"},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := w.Watch(ctx, req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond) // let the first snapshot arrive
		if _, err := coll.Doc("w1").Set(ctxT(t), map[string]any{"v": int64(1)}); err != nil {
			t.Fatal(err)
		}
		if _, err := coll.Doc("w1").Update(ctxT(t), []fs.Update{{Path: "v", Value: int64(2)}}); err != nil {
			t.Fatal(err)
		}
		if _, err := coll.Doc("w1").Delete(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		var ops []string
		timeout := time.After(10 * time.Second)
	loop:
		for len(ops) < 3 {
			select {
			case ev, ok := <-ch:
				if !ok {
					break loop
				}
				if ev.Err != nil {
					t.Fatalf("watch error: %v", ev.Err)
				}
				if ev.Key[0].Value.String() != "w1" || ev.Channel != "tobira_watch" {
					t.Errorf("event %+v", ev)
				}
				if ev.Op != "delete" {
					if o, _ := ev.Payload.AsObject(); len(o) != 1 || o[0].Key != "v" {
						t.Errorf("payload %s", ev.Payload)
					}
				}
				ops = append(ops, ev.Op)
			case <-timeout:
				break loop
			}
		}
		if !slices.Equal(ops, []string{"insert", "update", "delete"}) {
			t.Errorf("%+v: ops = %v", req, ops)
		}
		cancel()
		for range ch { // the channel closes once ctx ends
		}
	}
	if _, err := w.Watch(ctxT(t), driver.WatchRequest{}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("empty request: %v", err)
	}
}
