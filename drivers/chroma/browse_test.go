package chroma

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, c := range []browseCursor{
		{offset: 40},
		{sorted: true, spec: sortSpec([]driver.SortKey{{Field: "age", Desc: true}}), offset: 20},
		{sorted: true, spec: sortSpec([]driver.SortKey{{Field: "a|b c"}, {Field: "id", Desc: true}}), offset: 0},
	} {
		got, err := decodeCursor(c.encode())
		if err != nil {
			t.Fatalf("decode(%q): %v", c.encode(), err)
		}
		if got != c {
			t.Errorf("round trip %+v -> %q -> %+v", c, c.encode(), got)
		}
	}
	if c, err := decodeCursor(""); err != nil || c != (browseCursor{}) {
		t.Errorf("empty cursor = %+v, %v", c, err)
	}
	for _, s := range []string{"x", "o|", "o|-1", "o|a", "s|1", "s|-1|age%3Aa", "s|1|", "o|99999999999999999999", "s|2|age:a|x"} {
		if _, err := decodeCursor(s); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decodeCursor(%q): want ErrSyntax, got %v", s, err)
		}
	}
}

func TestCursorMatches(t *testing.T) {
	asc := sortSpec([]driver.SortKey{{Field: "age"}})
	desc := sortSpec([]driver.SortKey{{Field: "age", Desc: true}})
	if asc == desc {
		t.Fatal("asc and desc specs are equal")
	}
	c := browseCursor{sorted: true, spec: asc, offset: 20}
	if err := c.matches(asc); err != nil {
		t.Error(err)
	}
	for _, other := range []string{desc, ""} {
		if err := c.matches(other); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("cursor for %q used with %q: want ErrSyntax, got %v", asc, other, err)
		}
	}
	if err := (browseCursor{offset: 20}).matches(asc); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("unsorted cursor on a sorted browse: %v", err)
	}
}

func TestSortRecords(t *testing.T) {
	m := func(s string) map[string]any { return decodeMeta(t, s) }
	doc := func(s string) *string { return &s }
	recs := []record{
		{id: "10", metadata: m(`{"age":30,"name":"b"}`), document: doc("z")},
		{id: "2", metadata: m(`{"age":20.5,"name":"a"}`)},
		{id: "3", metadata: m(`{"name":"c"}`), document: doc("a")},
		{id: "1", metadata: m(`{"age":30}`)},
		{id: "11", metadata: m(`{"age":5}`)},
	}
	ids := func() string {
		var out []string
		for _, r := range recs {
			out = append(out, r.id)
		}
		return strings.Join(out, ",")
	}
	for _, tt := range []struct {
		keys []driver.SortKey
		want string
	}{
		// NULLs last in both directions; ties by id, numerically.
		{[]driver.SortKey{{Field: "age"}}, "11,2,1,10,3"},
		{[]driver.SortKey{{Field: "age", Desc: true}}, "1,10,2,11,3"},
		{[]driver.SortKey{{Field: "id"}}, "1,2,3,10,11"},
		{[]driver.SortKey{{Field: "id", Desc: true}}, "11,10,3,2,1"},
		{[]driver.SortKey{{Field: "document"}}, "3,10,1,2,11"},
		{[]driver.SortKey{{Field: "age", Desc: true}, {Field: "name", Desc: true}}, "10,1,2,11,3"},
	} {
		if err := sortRecords(recs, tt.keys); err != nil {
			t.Fatal(err)
		}
		if got := ids(); got != tt.want {
			t.Errorf("sort %+v = %s, want %s", tt.keys, got, tt.want)
		}
	}
	if err := sortRecords(recs, []driver.SortKey{{Field: "embedding"}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("sort by embedding: want ErrUnsupported, got %v", err)
	}
}

func TestBrowseAgainstFake(t *testing.T) {
	f := newFake(t)
	coll := f.addColl("default_database", "things", "l2", 2)
	for i := 1; i <= 25; i++ {
		coll.add(strconv.Itoa(i), []float64{float64(i), 0}, "", map[string]any{"n": 100 - i})
	}
	c := openFake(t, f, driver.ConnConfig{})
	ref := driver.CollectionRef{Name: "things"}
	var sizes []int
	cursor := ""
	for range 5 {
		rs, err := c.Browse(t.Context(), ref, driver.BrowseOptions{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		rows, st, err := driver.Collect(t.Context(), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(rows))
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if got := fmt.Sprint(sizes); got != "[10 10 5]" {
		t.Errorf("page sizes %v, want 10,10,5", sizes)
	}
	// Sorted: the whole collection is read without embeddings, then the
	// page's embeddings are fetched by id.
	rs, err := c.Browse(t.Context(), ref, driver.BrowseOptions{Limit: 3, Sort: []driver.SortKey{{Field: "n"}}})
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(t.Context(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || st.NextCursor == "" {
		t.Fatalf("sorted page: %d rows, cursor %q", len(rows), st.NextCursor)
	}
	ei := colIdx(cols, "embedding")
	if v, ok := rows[0][ei].AsVector(); !ok || v[0] != 25 {
		t.Errorf("first sorted row embedding = %s, want [25 0]", rows[0][ei])
	}
	if _, err := c.Browse(t.Context(), ref, driver.BrowseOptions{Limit: 3, Cursor: st.NextCursor}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("sorted cursor reused unsorted: want ErrSyntax, got %v", err)
	}
	if _, err := c.Browse(t.Context(), driver.CollectionRef{Name: "things", Namespace: "nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown namespace: want ErrNotFound, got %v", err)
	}
}

func colIdx(cols []driver.Column, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}
