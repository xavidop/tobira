package mongodb

import (
	"context"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

type fakeCatalog struct{ calls int }

func (f *fakeCatalog) collections(_ context.Context, ns string) ([]string, error) {
	f.calls++
	return []string{"orders", "tobira_people", "users"}, nil
}

func (f *fakeCatalog) fields(_ context.Context, ns, coll string) ([]string, error) {
	f.calls++
	if coll == "orders" {
		return []string{"_id", "status", "address", "address.city"}, nil
	}
	return nil, nil
}

func labels(cs []driver.Completion) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Label
	}
	return out
}

func TestComplete(t *testing.T) {
	cat := &fakeCatalog{}
	run := func(text string) []string {
		t.Helper()
		got, err := complete(context.Background(), cat, driver.CompletionRequest{Text: text, Cursor: len(text), Namespace: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		return labels(got)
	}
	if got := run(`{"find":"tobira_`); !slices.Equal(got, []string{"tobira_people"}) {
		t.Errorf("collection: %v", got)
	}
	if got := run(`{"aggregate":"`); !slices.Contains(got, "orders") || slices.Contains(got, "find") {
		t.Errorf("all collections: %v", got)
	}
	if got := run(`{"fi`); !slices.Contains(got, "find") || !slices.Contains(got, "filter") {
		t.Errorf("command: %v", got)
	}
	if got := run(`{"find":"orders","filter":{"addr`); !slices.Equal(got, []string{"address", "address.city"}) {
		t.Errorf("fields: %v", got)
	}
	if got := run(`{"find":"orders","filter":{"status":{"$i`); !slices.Contains(got, "$in") {
		t.Errorf("operator: %v", got)
	}
	if got := run(`{"aggregate":"orders","pipeline":[{"$gr`); !slices.Contains(got, "$group") {
		t.Errorf("stage: %v", got)
	}
	if got := run(`{"aggregate":"orders","pipeline":[{"$group":{"_id":"$sta`); !slices.Contains(got, "$status") {
		t.Errorf("field reference: %v", got)
	}
	got, _ := complete(context.Background(), cat, driver.CompletionRequest{Text: `{"find":"or`, Cursor: 100})
	if !slices.Contains(labels(got), "orders") {
		t.Errorf("cursor past the end is clamped: %v", labels(got))
	}
}

func TestCatalogCache(t *testing.T) {
	cat := &fakeCatalog{}
	c := newCatalogCacheFrom(cat, catalogTTL)
	ctx := context.Background()
	for range 3 {
		if _, err := c.collections(ctx, "shop"); err != nil {
			t.Fatal(err)
		}
	}
	if cat.calls != 1 {
		t.Errorf("collections loaded %d times, want 1", cat.calls)
	}
	c.Invalidate()
	_, _ = c.collections(ctx, "shop")
	if cat.calls != 2 {
		t.Errorf("after Invalidate: %d loads, want 2", cat.calls)
	}
}
