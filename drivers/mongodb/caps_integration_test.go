//go:build integration

package mongodb

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

func browseAll(t *testing.T, b driver.Browser, ref driver.CollectionRef, opts driver.BrowseOptions) ([][]driver.Row, []driver.Column) {
	t.Helper()
	var pages [][]driver.Row
	var cols []driver.Column
	for range 100 {
		rs, err := b.Browse(ctx30(t), ref, opts)
		if err != nil {
			t.Fatalf("Browse: %v", err)
		}
		cols = rs.Columns()
		rows, st, err := driver.Collect(ctx30(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, rows)
		if st.NextCursor == "" {
			return pages, cols
		}
		if !st.Truncated {
			t.Error("a page with a next cursor is Truncated")
		}
		opts.Cursor = st.NextCursor
	}
	t.Fatal("too many pages")
	return nil, nil
}

func TestIntegrationBrowse(t *testing.T) {
	c := openTest(t, testDB, false)
	b := c.(driver.Browser)
	big := driver.CollectionRef{Name: "tobira_big"}

	t.Run("keyset over _id", func(t *testing.T) {
		pages, _ := browseAll(t, b, big, driver.BrowseOptions{Limit: 400})
		var sizes []int
		var last int64
		for _, p := range pages {
			sizes = append(sizes, len(p))
			for _, r := range p {
				n, _ := r[0].AsInt()
				if n <= last {
					t.Fatalf("_id %d after %d", n, last)
				}
				last = n
			}
		}
		if !slices.Equal(sizes, []int{400, 400, 400, 300}) || last != 1500 {
			t.Errorf("page sizes %v, last %d", sizes, last)
		}
	})
	t.Run("page columns are the page's union", func(t *testing.T) {
		pages, cols := browseAll(t, b, big, driver.BrowseOptions{Limit: 1000})
		if len(pages) != 2 || !slices.Contains(colNames(cols), "late") {
			t.Errorf("last page columns %v", colNames(cols))
		}
		if cols[0].Source == nil || cols[0].Source.Namespace != testDB || !cols[0].Key {
			t.Errorf("_id column %+v", cols[0])
		}
	})
	t.Run("sort uses offsets", func(t *testing.T) {
		pages, _ := browseAll(t, b, big, driver.BrowseOptions{Limit: 700, Sort: []driver.SortKey{{Field: "n", Desc: true}}})
		if len(pages) != 3 || pages[0][0][0].String() != "1500" || pages[2][len(pages[2])-1][0].String() != "1" {
			t.Errorf("%d pages", len(pages))
		}
	})
	t.Run("mixed _id types page in BSON order", func(t *testing.T) {
		ctx := ctx30(t)
		coll := admin.Database(testDB).Collection("mixed_ids")
		if _, err := coll.InsertMany(ctx, []any{
			bson.D{{Key: "_id", Value: int32(2)}}, bson.D{{Key: "_id", Value: "b"}}, bson.D{{Key: "_id", Value: bson.NewObjectID()}},
			bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "_id", Value: "a"}}, bson.D{{Key: "_id", Value: true}},
			bson.D{{Key: "_id", Value: bson.NewDateTimeFromTime(time.Now())}},
		}); err != nil {
			t.Fatal(err)
		}
		pages, _ := browseAll(t, b, driver.CollectionRef{Name: "mixed_ids"}, driver.BrowseOptions{Limit: 2})
		n := 0
		for _, p := range pages {
			n += len(p)
		}
		if n != 7 {
			t.Errorf("saw %d documents over %d pages, want 7", n, len(pages))
		}
	})
	t.Run("filters", func(t *testing.T) {
		ref := driver.CollectionRef{Name: "tobira_people"}
		count := func(f driver.Filter) int {
			t.Helper()
			pages, _ := browseAll(t, b, ref, driver.BrowseOptions{Limit: 100, Filter: f})
			return len(pages[0])
		}
		if n := count(driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: driver.OpIsNull}}}); n != 5 {
			t.Errorf("age is null: %d", n)
		}
		if n := count(driver.Filter{Conditions: []driver.Condition{{Field: "name", Op: driver.OpPrefix, Value: driver.String("Ada")}}}); n != 4 {
			t.Errorf("name prefix Ada: %d", n)
		}
		if n := count(driver.Filter{Raw: `{"age":{"$gte":60}}`, Conditions: []driver.Condition{{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)}}}); n == 0 {
			t.Error("raw and condition matched nothing")
		}
		_, err := b.Browse(ctx30(t), ref, driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: `{"$out":"x"}`}})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only raw $out: want ErrReadOnly, got %v", err)
		}
	})
	t.Run("objectId filter typed as text", func(t *testing.T) {
		ctx := ctx30(t)
		oid := bson.NewObjectID()
		if _, err := admin.Database(testDB).Collection("oids").InsertOne(ctx, bson.D{{Key: "_id", Value: oid}}); err != nil {
			t.Fatal(err)
		}
		pages, _ := browseAll(t, b, driver.CollectionRef{Name: "oids"}, driver.BrowseOptions{
			Filter: driver.Filter{Conditions: []driver.Condition{{Field: "_id", Op: driver.OpEq, Value: driver.String(oid.Hex())}}}})
		if len(pages[0]) != 1 {
			t.Errorf("got %d rows", len(pages[0]))
		}
	})
	t.Run("bad cursor", func(t *testing.T) {
		_, err := b.Browse(ctx30(t), big, driver.BrowseOptions{Cursor: "o:5"})
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("offset cursor on a keyset browse: want ErrSyntax, got %v", err)
		}
	})
}

func TestIntegrationMutator(t *testing.T) {
	c := openTest(t, testDB, false)
	m := c.(driver.Mutator)
	ctx := ctx30(t)
	ref := driver.CollectionRef{Name: "edits"}
	mb := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }

	key, err := m.Insert(ctx, ref, driver.Object{mb("name", driver.String("a")), mb("addr", driver.ObjectValue(mb("city", driver.String("x"))))})
	if err != nil {
		t.Fatal(err)
	}
	id, ok := key.Get("_id")
	if native, _, _ := id.AsUnknown(); !ok || native != "objectId" {
		t.Fatalf("key %v: want an ObjectId _id", key)
	}
	if err := m.Update(ctx, ref, key, driver.Object{mb("addr", driver.ObjectValue(mb("city", driver.String("y")))), mb("name", driver.Null())}); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(ctx, ref, key, driver.Object{mb("addr.city", driver.String("z"))}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("dotted field name: want ErrUnsupported, got %v", err)
	}
	// A key field that no longer matches at write time is not written.
	if err := m.Update(ctx, ref, driver.Object{mb("_id", id), mb("name", driver.String("not-null"))}, driver.Object{mb("n", driver.Int(9))}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("stale key field: want ErrNotFound, got %v", err)
	}
	var got bson.D
	oid, _ := bson.ObjectIDFromHex(id.String())
	if err := admin.Database(testDB).Collection("edits").FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if name, has := lookup(got, "name"); !has || name != nil {
		t.Errorf("name set to NULL must be stored as null, got %v", got)
	}
	if addr, _ := lookup(got, "addr"); addr.(bson.D)[0].Value != "y" {
		t.Errorf("addr.city not set: %v", got)
	}
	// A key typed as the hex text of the ObjectId also matches.
	if err := m.Update(ctx, ref, driver.Object{mb("_id", driver.String(id.String()))}, driver.Object{mb("n", driver.Int(1))}); err != nil {
		t.Errorf("update by hex text: %v", err)
	}
	if err := m.Delete(ctx, ref, key); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"update missing": m.Update(ctx, ref, key, driver.Object{mb("n", driver.Int(2))}),
		"delete missing": m.Delete(ctx, ref, key),
	} {
		if !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", name, err)
		}
	}
	for range 2 {
		if _, err := m.Insert(ctx, ref, driver.Object{mb("dup", driver.Int(1))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Delete(ctx, ref, driver.Object{mb("dup", driver.Int(1))}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("ambiguous key: want ErrConflict, got %v", err)
	}
	if _, err := m.Insert(ctx, ref, driver.Object{mb("_id", driver.Int(5))}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Insert(ctx, ref, driver.Object{mb("_id", driver.Int(5))}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("duplicate _id: want ErrConstraint, got %v", err)
	}

	ro := openTest(t, testDB, true).(driver.Mutator)
	k5 := driver.Object{mb("_id", driver.Int(5))}
	_, insErr := ro.Insert(ctx, ref, driver.Object{mb("x", driver.Int(1))})
	for name, err := range map[string]error{
		"insert": insErr,
		"update": ro.Update(ctx, ref, k5, driver.Object{mb("x", driver.Int(1))}),
		"delete": ro.Delete(ctx, ref, k5),
	} {
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %s: want ErrReadOnly, got %v", name, err)
		}
	}
}

func TestIntegrationExplain(t *testing.T) {
	c := openTest(t, testDB, false)
	x := c.(driver.Explainer)
	ctx := ctx30(t)
	plan, err := x.Explain(ctx, driver.Query{Text: `{"find":"tobira_people","filter":{"email":"user07@example.com"}}`}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Format != "json" || !strings.Contains(plan.Text, "winningPlan") || plan.Root == nil {
		t.Fatalf("plan %+v", plan)
	}
	var labels []string
	var walk func(n *driver.PlanNode)
	walk = func(n *driver.PlanNode) {
		labels = append(labels, n.Label+":"+n.Detail)
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(plan.Root)
	if !slices.ContainsFunc(labels, func(l string) bool { return strings.HasSuffix(l, "IXSCAN:email_unique") }) {
		t.Errorf("plan nodes %v", labels)
	}
	plan, err = x.Explain(ctx, driver.Query{Text: `{"aggregate":"tobira_people","pipeline":[{"$match":{"active":true}},{"$group":{"_id":"$age"}}]}`, ReadOnly: true}, driver.ExplainOptions{Analyze: true})
	if err != nil || !strings.Contains(plan.Text, "executionStats") && !strings.Contains(plan.Text, "executionTimeMillis") {
		t.Errorf("analyze aggregate: %v %v", err, plan)
	}
	del := driver.Query{Text: `{"delete":"tobira_people","deletes":[{"q":{},"limit":0}]}`}
	if _, err := x.Explain(ctx, driver.Query{Text: del.Text, ReadOnly: true}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only analyze of a delete: want ErrReadOnly, got %v", err)
	}
	// On a writable call, explaining a delete with executionStats never applies it.
	if _, err := x.Explain(ctx, del, driver.ExplainOptions{Analyze: true}); err != nil {
		t.Fatal(err)
	}
	if _, rows, _ := collect(t, c, driver.Query{Text: `{"count":"tobira_people"}`}); rows[0][0].String() != "57" {
		t.Errorf("count after explaining a delete: %v", rows)
	}
}

func TestIntegrationComplete(t *testing.T) {
	c := openTest(t, testDB, false)
	comp := c.(driver.Completer)
	text := `{"find":"docs","filter":{"address.`
	got, err := comp.Complete(ctx30(t), driver.CompletionRequest{Text: text, Cursor: len(text)})
	if err != nil {
		t.Fatal(err)
	}
	if ls := labels(got); !slices.Contains(ls, "address.city") || !slices.Contains(ls, "address.geo.lat") {
		t.Errorf("completions %v", ls)
	}
	c.(driver.CatalogInvalidator).Invalidate()
}

func TestIntegrationWatch(t *testing.T) {
	c := openTest(t, testDB, false)
	w := c.(driver.Watcher)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := w.Watch(ctx, driver.WatchRequest{Collection: &driver.CollectionRef{Name: "watched"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Database(testDB).Collection("watched").InsertOne(ctx30(t), bson.D{{Key: "_id", Value: int32(42)}, {Key: "v", Value: "x"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Err != nil || ev.Op != "insert" || ev.Channel != testDB+".watched" || ev.Payload.String() != `{"_id":42,"v":"x"}` {
			t.Errorf("event %+v", ev)
		}
		if id, _ := ev.Key.Get("_id"); id.String() != "42" {
			t.Errorf("key %v", ev.Key)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
	cancel()
	for range ch {
	}
	if _, err := w.Watch(context.Background(), driver.WatchRequest{}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("empty request: want ErrSyntax, got %v", err)
	}
}

// TestIntegrationWatchStandalone runs a standalone MongoDB (no replica
// set), where change streams are ErrUnsupported with a hint.
func TestIntegrationWatchStandalone(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcmongo.Run(ctx, mongoImage())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New().Open(ctx30(t), driver.ConnConfig{DSN: uri + "tobira?directConnection=true"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.(driver.Watcher).Watch(ctx30(t), driver.WatchRequest{Collection: &driver.CollectionRef{Name: "x"}})
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), changeStreamHint) {
		t.Errorf("want ErrUnsupported with a hint, got %v", err)
	}
}
