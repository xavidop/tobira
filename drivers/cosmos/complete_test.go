package cosmos

import (
	"context"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

type fakeCatalog struct{ calls int }

func (f *fakeCatalog) containers(_ context.Context, db string) ([]string, error) {
	f.calls++
	return []string{"orders", "tobira_people", "users"}, nil
}

func (f *fakeCatalog) fields(_ context.Context, db, container string) ([]string, error) {
	f.calls++
	if container == "orders" {
		return []string{"id", "status", "address", "address.city"}, nil
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
	if got := run(`SELECT * FROM tobira_`); !slices.Equal(got, []string{"tobira_people"}) {
		t.Errorf("container: %v", got)
	}
	if got := run(`select * from `); !slices.Equal(got, []string{"orders", "tobira_people", "users"}) {
		t.Errorf("all containers: %v", got)
	}
	if got := run(`SEL`); !slices.Contains(got, "SELECT") {
		t.Errorf("keyword: %v", got)
	}
	if got := run(`SELECT * FROM orders o WHERE STARTS`); !slices.Contains(got, "STARTSWITH") {
		t.Errorf("function: %v", got)
	}
	if got := run(`SELECT * FROM orders o WHERE o.st`); !slices.Equal(got, []string{"status"}) {
		t.Errorf("field: %v", got)
	}
	if got := run(`SELECT * FROM orders o WHERE o.address.c`); !slices.Equal(got, []string{"city"}) {
		t.Errorf("nested field: %v", got)
	}
	if got := run(`{"container":"tob`); !slices.Equal(got, []string{"tobira_people"}) {
		t.Errorf("envelope container: %v", got)
	}
	if got := run(`SELECT * FROM orders o WHERE o.name = 'tob`); len(got) != 0 {
		t.Errorf("inside a string literal: %v", got)
	}
	got, err := complete(context.Background(), cat, driver.CompletionRequest{Text: "SELECT * FROM o", Cursor: 99})
	if err != nil || len(got) != 0 {
		t.Errorf("cursor past the end: %v %v", got, err)
	}
}

func TestCatalogCacheInvalidate(t *testing.T) {
	src := &fakeCatalog{}
	cache := newCatalogCache(src, catalogTTL)
	ctx := context.Background()
	for range 3 {
		if _, err := cache.containers(ctx, "shop"); err != nil {
			t.Fatal(err)
		}
	}
	if src.calls != 1 {
		t.Errorf("cached lookups made %d calls, want 1", src.calls)
	}
	cache.Invalidate()
	if _, err := cache.containers(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if src.calls != 2 {
		t.Errorf("after Invalidate: %d calls, want 2", src.calls)
	}
}
