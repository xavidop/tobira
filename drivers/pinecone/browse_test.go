package pinecone

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, c := range []browseCursor{
		{},
		{token: "abc==/+&|x", skip: 3},
		{token: "t"},
		{sorted: true, key: "age", desc: true, offset: 40},
		{sorted: true, key: "we|ird key", offset: 0},
	} {
		s := c.encode()
		got, err := decodeCursor(s)
		if err != nil {
			t.Errorf("decode(%q): %v", s, err)
			continue
		}
		if got != c {
			t.Errorf("round trip %+v -> %q -> %+v", c, s, got)
		}
	}
	if (browseCursor{}).encode() != "" {
		t.Error("the first page has an empty cursor")
	}
	for _, bad := range []string{"x", "l|a", "l|-1|t", "s|x|1|age", "s|a|z|age", "s|a|1|", "l|1|%zz", "s|a|-2|age"} {
		if _, err := decodeCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decode(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}

func TestSortRecords(t *testing.T) {
	recs := []record{
		{id: "a", meta: map[string]driver.Value{"age": driver.Int(30)}},
		{id: "b", meta: map[string]driver.Value{}},
		{id: "c", meta: map[string]driver.Value{"age": driver.Int(20)}},
		{id: "d", meta: map[string]driver.Value{"age": driver.Int(30)}},
		{id: "e", meta: map[string]driver.Value{"age": driver.Float(25.5)}},
	}
	ids := func(rs []record) string {
		s := ""
		for _, r := range rs {
			s += r.id
		}
		return s
	}
	asc := append([]record(nil), recs...)
	sortRecords(asc, "age", false)
	if got := ids(asc); got != "ceadb" {
		t.Errorf("asc = %s", got)
	}
	desc := append([]record(nil), recs...)
	sortRecords(desc, "age", true)
	// Ties keep id order; records without the field come last either way.
	if got := ids(desc); got != "adecb" {
		t.Errorf("desc = %s", got)
	}
	byID := append([]record(nil), recs...)
	sortRecords(byID, idColumn, true)
	if got := ids(byID); got != "edcba" {
		t.Errorf("by _id desc = %s", got)
	}
}

// Browse checks its options before contacting the server.
func TestBrowseRefusesBeforeNetwork(t *testing.T) {
	c := newTestConn(t, false)
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "default", Name: "people"}
	for name, opts := range map[string]driver.BrowseOptions{
		"raw filter":   {Filter: driver.Filter{Raw: `{"a":1}`}},
		"two keys":     {Sort: []driver.SortKey{{Field: "a"}, {Field: "b"}}},
		"vector sort":  {Sort: []driver.SortKey{{Field: valuesColumn}}},
		"sparse sort":  {Sort: []driver.SortKey{{Field: sparseColumn}}},
		"unknown op":   {Filter: driver.Filter{Conditions: []driver.Condition{{Field: "a", Op: "regex", Value: driver.String("x")}}}},
		"bad cursor":   {Cursor: "nope"},
		"sort cursor":  {Cursor: (browseCursor{sorted: true, key: "age", offset: 20}).encode()},
		"other sort":   {Cursor: (browseCursor{sorted: true, key: "age", offset: 20}).encode(), Sort: []driver.SortKey{{Field: "name"}}},
		"other dir":    {Cursor: (browseCursor{sorted: true, key: "age", offset: 20}).encode(), Sort: []driver.SortKey{{Field: "age", Desc: true}}},
		"plain cursor": {Cursor: (browseCursor{token: "t"}).encode(), Sort: []driver.SortKey{{Field: "age"}}},
	} {
		_, err := c.Browse(ctx, ref, opts)
		if !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%s: %v, want ErrUnsupported or ErrSyntax", name, err)
		}
	}
	if _, err := c.Browse(ctx, driver.CollectionRef{Namespace: "other", Name: "people"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown index: %v", err)
	}
}
