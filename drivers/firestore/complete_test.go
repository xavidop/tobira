package firestore

import (
	"context"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

type fakeCatalog []string

func (f fakeCatalog) collections(context.Context) ([]string, error) { return f, nil }

func labels(cs []driver.Completion) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Label
	}
	return out
}

func TestComplete(t *testing.T) {
	cat := fakeCatalog{"orders", "tobira_people", "users"}
	tests := []struct {
		text string
		want []string
		not  []string
	}{
		{`{"collection":"tobira_`, []string{"tobira_people"}, []string{"orders", "collection"}},
		{`{"collectionGroup":"o`, []string{"orders"}, nil},
		{`{"document":"us`, []string{"users"}, nil},
		{`{"col`, []string{"collection", "collectionGroup"}, []string{"orders"}},
		{`{"collection":"a","or`, []string{"orderBy"}, nil},
		{`{"collection":"a","where":[["x","arr`, []string{"array-contains", "array-contains-any"}, nil},
		{`{"collection":"a","where":[["x","n`, []string{"not-in"}, nil},
	}
	for _, tt := range tests {
		got, err := complete(context.Background(), cat, driver.CompletionRequest{Text: tt.text, Cursor: len(tt.text)})
		if err != nil {
			t.Fatalf("%s: %v", tt.text, err)
		}
		l := labels(got)
		for _, w := range tt.want {
			if !slices.Contains(l, w) {
				t.Errorf("%s: no %q in %v", tt.text, w, l)
			}
		}
		for _, n := range tt.not {
			if slices.Contains(l, n) {
				t.Errorf("%s: unexpected %q in %v", tt.text, n, l)
			}
		}
	}
	// The cursor may sit mid-text and out of range without a panic.
	for _, cur := range []int{-1, 0, 3, 1000} {
		if _, err := complete(context.Background(), cat, driver.CompletionRequest{Text: `{"collection":"x"}`, Cursor: cur}); err != nil {
			t.Errorf("cursor %d: %v", cur, err)
		}
	}
}
