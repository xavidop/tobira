package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestFieldPath(t *testing.T) {
	for _, tc := range []struct{ field, want string }{
		{"age", `c["age"]`},
		{"addr.city", `c["addr"]["city"]`},
		{`we"ird`, `c["we\"ird"]`},
	} {
		got, err := fieldPath(tc.field)
		if err != nil || got != tc.want {
			t.Errorf("fieldPath(%q) = %q, %v; want %q", tc.field, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "a..b", ".a", "a."} {
		if _, err := fieldPath(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("fieldPath(%q): want ErrSyntax, got %v", bad, err)
		}
	}
}

func TestBrowseSQL(t *testing.T) {
	sql, params, err := browseSQL(driver.Filter{
		Conditions: []driver.Condition{
			{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)},
			{Field: "age", Op: driver.OpGe, Value: driver.Int(30)},
			{Field: "name", Op: driver.OpPrefix, Value: driver.String("A")},
			{Field: "id", Op: driver.OpContains, Value: driver.String("1")},
			{Field: "tier", Op: driver.OpIn, Value: driver.List(driver.String("a"), driver.String("b"))},
			{Field: "gone", Op: driver.OpIsNull},
			{Field: "here", Op: driver.OpNotNull},
			{Field: "x", Op: driver.OpNe, Value: driver.Int(1)},
			{Field: "nothing", Op: driver.OpEq, Value: driver.Null()},
		},
		Raw: "c.score > 10",
	}, []driver.SortKey{{Field: "age", Desc: true}, {Field: "name"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT * FROM c WHERE c["active"] = @w1 AND c["age"] >= @w2 AND STARTSWITH(c["name"], @w3) AND CONTAINS(c["id"], @w4)` +
		` AND ARRAY_CONTAINS(@w5, c["tier"]) AND (NOT IS_DEFINED(c["gone"]) OR IS_NULL(c["gone"]))` +
		` AND (IS_DEFINED(c["here"]) AND NOT IS_NULL(c["here"])) AND c["x"] != @w6 AND (NOT IS_DEFINED(c["nothing"]) OR IS_NULL(c["nothing"]))` +
		` AND (c.score > 10) ORDER BY c["age"] DESC, c["name"] ASC`
	if sql != want {
		t.Errorf("browseSQL:\n got %s\nwant %s", sql, want)
	}
	if len(params) != 6 || params[4].Name != "@w5" || string(params[4].Value.(json.RawMessage)) != `["a","b"]` {
		t.Errorf("params = %+v", params)
	}
	if sql, params, err := browseSQL(driver.Filter{}, nil, false); err != nil || sql != "SELECT * FROM c" || len(params) != 0 {
		t.Errorf("empty browse = %q %v %v", sql, params, err)
	}
	for _, tc := range []struct {
		f    driver.Filter
		kind error
	}{
		{driver.Filter{Conditions: []driver.Condition{{Field: "a", Op: "regex", Value: driver.String("x")}}}, driver.ErrUnsupported},
		{driver.Filter{Conditions: []driver.Condition{{Field: "a", Op: driver.OpContains, Value: driver.Int(1)}}}, driver.ErrUnsupported},
		{driver.Filter{Conditions: []driver.Condition{{Field: "a", Op: driver.OpIn, Value: driver.Int(1)}}}, driver.ErrSyntax},
		{driver.Filter{Conditions: []driver.Condition{{Field: "", Op: driver.OpEq, Value: driver.Int(1)}}}, driver.ErrSyntax},
		{driver.Filter{Raw: "true) OR (true"}, driver.ErrSyntax},
	} {
		if _, _, err := browseSQL(tc.f, nil, false); !errors.Is(err, tc.kind) {
			t.Errorf("browseSQL(%+v): want %v, got %v", tc.f, tc.kind, err)
		}
	}
	if _, _, err := browseSQL(driver.Filter{Raw: "udf.f(c)"}, nil, true); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only browse with a UDF raw filter: want ErrReadOnly, got %v", err)
	}
}

func TestCursorCodec(t *testing.T) {
	pos := browsePos{Token: `{"token":"-RID:~abc#RT:1"}`, Skip: 3}
	cur := encodeBrowseCursor("sig-a", pos)
	if strings.ContainsAny(cur, "+/= ") {
		t.Errorf("cursor %q is not URL-safe", cur)
	}
	got, err := decodeBrowseCursor(cur, "sig-a")
	if err != nil || got != pos {
		t.Errorf("decodeBrowseCursor = %+v, %v; want %+v", got, err, pos)
	}
	if got, err := decodeBrowseCursor(encodeBrowseCursor("sig-a", browsePos{Skip: 20}), "sig-a"); err != nil || got.Skip != 20 {
		t.Errorf("a cursor inside the first server page = %+v, %v", got, err)
	}
	if _, err := decodeBrowseCursor(cur, "sig-b"); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("a cursor for another browse: want ErrSyntax, got %v", err)
	}
	for _, bad := range []string{"!!!", "e30", encodeBrowseCursor("sig-a", browsePos{}), encodeBrowseCursor("sig-a", browsePos{Token: "x", Skip: -1})} {
		if _, err := decodeBrowseCursor(bad, "sig-a"); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decodeBrowseCursor(%q): want ErrSyntax, got %v", bad, err)
		}
	}
	a := driver.CollectionRef{Namespace: "shop", Name: "orders"}
	if browseSignature(a, "SELECT 1", nil) == browseSignature(a, "SELECT 2", nil) {
		t.Error("different browses share a signature")
	}
	for _, other := range []driver.CollectionRef{{Namespace: "shop", Name: "users"}, {Namespace: "crm", Name: "orders"}, {Namespace: "shoporders"}} {
		if browseSignature(a, "SELECT 1", nil) == browseSignature(other, "SELECT 1", nil) {
			t.Errorf("a cursor for %+v would be accepted on %+v", a, other)
		}
	}
}

// fakePages serves server pages of item ids; the token is the page number.
func fakePages(pages [][]int) pageFetch {
	return func(_ context.Context, token string, _ int) ([][]byte, string, float64, error) {
		n := 0
		if token != "" {
			n, _ = strconv.Atoi(token)
		}
		var items [][]byte
		for _, id := range pages[n] {
			items = append(items, []byte(`{"id":"`+strconv.Itoa(id)+`"}`))
		}
		next := ""
		if n+1 < len(pages) {
			next = strconv.Itoa(n + 1)
		}
		return items, next, 1, nil
	}
}

func ids(t *testing.T, items []driver.Value) []string {
	t.Helper()
	var out []string
	for _, it := range items {
		o, _ := it.AsObject()
		v, _ := o.Get("id")
		s, _ := v.AsText()
		out = append(out, s)
	}
	return out
}

func TestBrowseItemsPaging(t *testing.T) {
	ctx := context.Background()
	fetch := fakePages([][]int{{1, 2, 3, 4, 5}, {6, 7}, {8, 9, 10}})
	var got []string
	pos := browsePos{}
	for range 10 {
		items, next, _, err := browseItems(ctx, fetch, pos, 3)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ids(t, items)...)
		if next == nil {
			break
		}
		pos = *next
	}
	if strings.Join(got, ",") != "1,2,3,4,5,6,7,8,9,10" {
		t.Errorf("paged ids = %v", got)
	}
	// A page served smaller than when the cursor was made (the service
	// shrinks pages under its RU and time budgets): the skip carries over
	// to the next page instead of failing.
	items, next, _, err := browseItems(ctx, fakePages([][]int{{1, 2}, {3, 4, 5}, {6}}), browsePos{Skip: 3}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(t, items); strings.Join(g, ",") != "4,5" || next == nil || next.Token != "2" || next.Skip != 0 {
		t.Errorf("resume with a stale skip = %v, next %+v; want 4,5 then page 2", g, next)
	}
	items, next, _, err = browseItems(ctx, fakePages([][]int{{1, 2}}), browsePos{Skip: 5}, 2)
	if err != nil || len(items) != 0 || next != nil {
		t.Errorf("a skip past the end = %v, %+v, %v; want no items and no cursor", items, next, err)
	}
}
