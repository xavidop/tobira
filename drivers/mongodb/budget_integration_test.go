//go:build integration

package mongodb

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

// withBudget lowers the byte budget for one test.
func withBudget(t *testing.T, n int) {
	t.Helper()
	old := maxResultBytes
	maxResultBytes = n
	t.Cleanup(func() { maxResultBytes = old })
}

// seedFat fills tobira.fat with 20 documents of about 50 KB each; the last
// one holds an extra field.
func seedFat(t *testing.T) {
	t.Helper()
	ctx := ctx30(t)
	coll := admin.Database(testDB).Collection("fat")
	_ = coll.Drop(ctx)
	docs := make([]any, 20)
	for i := range docs {
		d := bson.D{{Key: "_id", Value: int32(i + 1)}, {Key: "blob", Value: strings.Repeat("x", 50_000)}}
		if i == 19 {
			d = append(d, bson.E{Key: "late", Value: true})
		}
		docs[i] = d
	}
	if _, err := coll.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationByteBudget(t *testing.T) {
	seedFat(t)
	withBudget(t, 120_000)
	c := openTest(t, testDB, false)
	ref := driver.CollectionRef{Name: "fat"}

	t.Run("browse pages stop at the budget", func(t *testing.T) {
		pages, _ := browseAll(t, c.(driver.Browser), ref, driver.BrowseOptions{Limit: 100})
		total := 0
		for _, p := range pages {
			if len(p) > 3 {
				t.Errorf("a page held %d documents of 50 KB under a 120 KB budget", len(p))
			}
			total += len(p)
		}
		if total != 20 {
			t.Errorf("paged %d documents, want 20", total)
		}
		rs, err := c.(driver.Browser).Browse(ctx30(t), ref, driver.BrowseOptions{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		_, st, _ := driver.Collect(ctx30(t), rs, 0)
		if !st.Truncated || st.NextCursor == "" || len(st.Warnings) == 0 {
			t.Errorf("stats %+v: want Truncated, a next cursor and a warning", st)
		}
	})
	t.Run("describe sample stops at the budget", func(t *testing.T) {
		sch, err := c.Describe(ctx30(t), ref)
		if err != nil {
			t.Fatal(err)
		}
		if len(sch.Sample) > 3 || !strings.Contains(sch.Comment, "MiB") && !strings.Contains(sch.Comment, "bytes") {
			t.Errorf("sample %d documents, comment %q", len(sch.Sample), sch.Comment)
		}
	})
	t.Run("query prefetch stops at the budget and still streams every row", func(t *testing.T) {
		cols, rows, st := collect(t, c, driver.Query{Text: `{"find":"fat","sort":{"_id":1}}`})
		if len(rows) != 20 {
			t.Errorf("%d rows, want 20", len(rows))
		}
		if len(cols) != 2 || len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "1 fields") {
			t.Errorf("columns %v warnings %q", colNames(cols), st.Warnings)
		}
	})
}

func TestIntegrationCollectionsCap(t *testing.T) {
	old := maxCollections
	maxCollections = 2
	t.Cleanup(func() { maxCollections = old })
	c := openTest(t, testDB, false)
	colls, err := c.Collections(ctx30(t), driver.NamespaceRef{Name: testDB})
	if err != nil {
		t.Fatal(err)
	}
	if len(colls) != 2 || !strings.Contains(colls[len(colls)-1].Comment, "stopped at 2 collections") {
		t.Errorf("collections %+v: want 2 with a note on the last", colls)
	}
}
