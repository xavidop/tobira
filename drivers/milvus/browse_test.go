package milvus

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, c := range []browseCursor{
		{},
		{after: driver.Int(42)},
		{after: driver.Int(-7)},
		{after: driver.String(`a|b%c"d`)},
		{sorted: true, field: "age", desc: true, offset: 40},
		{sorted: true, field: "$meta.x|y", offset: 0},
	} {
		got, err := decodeCursor(c.encode())
		if err != nil {
			t.Fatalf("decode(%q): %v", c.encode(), err)
		}
		if got.encode() != c.encode() {
			t.Errorf("round trip %q -> %q", c.encode(), got.encode())
		}
	}
}

func TestCursorRefusals(t *testing.T) {
	for _, s := range []string{"x", "k|", "k|q:1", "k|i:abc", "s|x|1|age", "s|a|-1|age", "s|a|1|", "s|a|1", "k|s:%zz", "s|a|99999999999999999999|age"} {
		if _, err := decodeCursor(s); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decodeCursor(%q) = %v, want ErrSyntax", s, err)
		}
	}
	c := browseCursor{sorted: true, field: "age", desc: true}
	if err := c.matches("age", false); !errors.Is(err, driver.ErrSyntax) {
		t.Error("a cursor reused with another direction must be refused")
	}
	if err := c.matches("", false); !errors.Is(err, driver.ErrSyntax) {
		t.Error("a sorted cursor reused unsorted must be refused")
	}
	if err := (browseCursor{after: driver.Int(1)}).matches("age", false); !errors.Is(err, driver.ErrSyntax) {
		t.Error("a keyset cursor reused sorted must be refused")
	}
	if err := c.matches("age", true); err != nil {
		t.Errorf("matching cursor: %v", err)
	}
}

func TestSortRows(t *testing.T) {
	rows := []driver.Row{
		{driver.Int(1), driver.Int(30)},
		{driver.Int(2), driver.Null()},
		{driver.Int(3), driver.Int(10)},
		{driver.Int(4), driver.Int(30)},
		{driver.Int(5), driver.String("x")},
	}
	sortRows(rows, 1, 0, true)
	var ids []int64
	for _, r := range rows {
		n, _ := r[0].AsInt()
		ids = append(ids, n)
	}
	// desc: 30, 30 (ties by key), 10, then the text, then NULLs last
	want := []int64{1, 4, 3, 5, 2}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("sorted ids = %v, want %v", ids, want)
		}
	}
	sortRows(rows, 1, 0, false)
	ids = ids[:0]
	for _, r := range rows {
		n, _ := r[0].AsInt()
		ids = append(ids, n)
	}
	want = []int64{3, 1, 4, 5, 2}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("asc sorted ids = %v, want %v", ids, want)
		}
	}
}

func TestBrowseRefusalsBeforeNetwork(t *testing.T) {
	c, fake := newFakeConn(t, false)
	fake.describe = describeOK(peopleProto())
	for name, opts := range map[string]driver.BrowseOptions{
		"two sort keys": {Sort: []driver.SortKey{{Field: "age"}, {Field: "name"}}},
		"vector sort":   {Sort: []driver.SortKey{{Field: "embedding"}}},
		"bad cursor":    {Cursor: "zzz"},
		"bad filter":    {Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpContains, Value: driver.String("1")}}}},
	} {
		_, err := c.Browse(t.Context(), driver.CollectionRef{Name: "people"}, opts)
		if err == nil {
			t.Errorf("%s: Browse succeeded", name)
		}
	}
	for _, call := range fake.Calls() {
		if call != "DescribeCollection" {
			t.Errorf("a refused browse sent %s", call)
		}
	}
}
