package elasticsearch

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func testMapping(t *testing.T) *mapping {
	t.Helper()
	m, _, err := parseMappings([]byte(peopleMapping))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBuildQuery(t *testing.T) {
	m := testMapping(t)
	cond := func(f string, op driver.Op, v driver.Value) driver.Filter {
		return driver.Filter{Conditions: []driver.Condition{{Field: f, Op: op, Value: v}}}
	}
	for _, tc := range []struct {
		name string
		f    driver.Filter
		want string
	}{
		{"empty", driver.Filter{}, `{"match_all":{}}`},
		{"eq bool", cond("active", driver.OpEq, driver.Bool(true)), `{"bool":{"filter":[{"term":{"active":true}}]}}`},
		{"eq text uses keyword", cond("name", driver.OpEq, driver.String("Ada")), `{"bool":{"filter":[{"term":{"name.keyword":"Ada"}}]}}`},
		{"eq null", cond("age", driver.OpEq, driver.Null()), `{"bool":{"must_not":[{"exists":{"field":"age"}}]}}`},
		{"ne", cond("id", driver.OpNe, driver.Int(3)), `{"bool":{"must_not":[{"term":{"id":3}}]}}`},
		{"lt", cond("age", driver.OpLt, driver.Int(30)), `{"bool":{"filter":[{"range":{"age":{"lt":30}}}]}}`},
		{"ge decimal", cond("score", driver.OpGe, driver.Decimal("1.50")), `{"bool":{"filter":[{"range":{"score":{"gte":1.50}}}]}}`},
		{"contains keyword", cond("email", driver.OpContains, driver.String("ex*a?")), `{"bool":{"filter":[{"wildcard":{"email":{"value":"*ex\\*a\\?*"}}}]}}`},
		{"contains text", cond("name", driver.OpContains, driver.String("Ada")), `{"bool":{"filter":[{"match_phrase":{"name":"Ada"}}]}}`},
		{"prefix keyword", cond("email", driver.OpPrefix, driver.String("user0")), `{"bool":{"filter":[{"prefix":{"email":"user0"}}]}}`},
		{"in", cond("id", driver.OpIn, driver.List(driver.Int(1), driver.Int(2))), `{"bool":{"filter":[{"terms":{"id":[1,2]}}]}}`},
		{"is null", cond("age", driver.OpIsNull, driver.Null()), `{"bool":{"must_not":[{"exists":{"field":"age"}}]}}`},
		{"not null", cond("age", driver.OpNotNull, driver.Null()), `{"bool":{"filter":[{"exists":{"field":"age"}}]}}`},
		{"nested path", cond("author.name", driver.OpEq, driver.String("Ada")), `{"bool":{"filter":[{"term":{"author.name":"Ada"}}]}}`},
		{"raw", driver.Filter{Raw: `{"match":{"name":"ada"}}`}, `{"bool":{"filter":[{"match":{"name":"ada"}}]}}`},
	} {
		got, err := buildQuery(tc.f, m, false)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if s := jsonOf(t, got); s != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, s, tc.want)
		}
	}
}

func TestBuildQueryRefusals(t *testing.T) {
	m := testMapping(t)
	for name, tc := range map[string]struct {
		f        driver.Filter
		readOnly bool
		kind     error
	}{
		"contains on a number":    {driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpContains, Value: driver.String("1")}}}, false, driver.ErrUnsupported},
		"prefix on a date":        {driver.Filter{Conditions: []driver.Condition{{Field: "created_at", Op: driver.OpPrefix, Value: driver.String("2026")}}}, false, driver.ErrUnsupported},
		"in without a list":       {driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.Int(1)}}}, false, driver.ErrSyntax},
		"unknown op":              {driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: "regex", Value: driver.Int(1)}}}, false, driver.ErrUnsupported},
		"raw not an object":       {driver.Filter{Raw: `[1]`}, false, driver.ErrSyntax},
		"raw malformed":           {driver.Filter{Raw: `{"match":`}, false, driver.ErrSyntax},
		"raw duplicate keys":      {driver.Filter{Raw: `{"match":{"a":1},"match":{"b":1}}`}, false, driver.ErrSyntax},
		"raw script read-only":    {driver.Filter{Raw: `{"script":{"script":"1"}}`}, true, driver.ErrReadOnly},
		"raw malformed read-only": {driver.Filter{Raw: `{"match":`}, true, driver.ErrReadOnly},
	} {
		_, err := buildQuery(tc.f, m, tc.readOnly)
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: want %v, got %v", name, tc.kind, err)
		}
	}
	// A script in a raw filter is allowed when the call is not read-only.
	if _, err := buildQuery(driver.Filter{Raw: `{"script":{"script":"1"}}`}, m, false); err != nil {
		t.Errorf("writable raw script filter: %v", err)
	}
}

func TestBuildSort(t *testing.T) {
	m := testMapping(t)
	got, err := buildSort([]driver.SortKey{{Field: "age", Desc: true}, {Field: "name"}}, m)
	if err != nil {
		t.Fatal(err)
	}
	if s := jsonOf(t, got); s != `[{"age":{"order":"desc"}},{"name.keyword":{"order":"asc"}}]` {
		t.Errorf("sort = %s", s)
	}
	if _, err := buildSort([]driver.SortKey{{Field: "tags"}}, m); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("sorting on a disabled object: want ErrUnsupported, got %v", err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := browseCursor{PIT: "pit-id==", After: json.RawMessage(`[33,"x"]`)}
	s, err := c.encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCursor(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.PIT != c.PIT || string(got.After) != string(c.After) {
		t.Errorf("round trip = %+v", got)
	}
	for _, bad := range []string{"%%%", "e30", "bm90IGpzb24"} {
		if _, err := decodeCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decodeCursor(%q): want ErrSyntax, got %v", bad, err)
		}
	}
}
