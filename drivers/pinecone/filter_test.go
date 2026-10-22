package pinecone

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestMatchConditions(t *testing.T) {
	r := record{id: "doc-12", meta: map[string]driver.Value{
		"age":    driver.Int(30),
		"score":  driver.Float(1.5),
		"name":   driver.String("Ada Lovelace"),
		"active": driver.Bool(true),
		"tags":   driver.List(driver.String("admin"), driver.String("beta")),
	}}
	cond := func(f string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: f, Op: op, Value: v}
	}
	for _, tc := range []struct {
		c    driver.Condition
		want bool
	}{
		{cond("age", driver.OpEq, driver.Int(30)), true},
		{cond("age", driver.OpEq, driver.Float(30)), true},
		{cond("age", driver.OpEq, driver.Decimal("30.0")), true},
		{cond("age", driver.OpEq, driver.String("30")), false},
		{cond("age", driver.OpNe, driver.Int(31)), true},
		{cond("age", driver.OpLt, driver.Int(31)), true},
		{cond("age", driver.OpLe, driver.Int(30)), true},
		{cond("age", driver.OpGt, driver.Int(30)), false},
		{cond("score", driver.OpGe, driver.Float(1.5)), true},
		{cond("name", driver.OpEq, driver.String("Ada Lovelace")), true},
		{cond("name", driver.OpLt, driver.String("B")), true},
		{cond("name", driver.OpContains, driver.String("Love")), true},
		{cond("name", driver.OpPrefix, driver.String("Ada")), true},
		{cond("name", driver.OpPrefix, driver.String("Love")), false},
		{cond("active", driver.OpEq, driver.Bool(true)), true},
		{cond("active", driver.OpEq, driver.Bool(false)), false},
		{cond("tags", driver.OpContains, driver.String("beta")), true},
		{cond("tags", driver.OpContains, driver.String("bet")), false},
		{cond("age", driver.OpIn, driver.List(driver.Int(1), driver.Int(30))), true},
		{cond("age", driver.OpIn, driver.List(driver.Int(1))), false},
		{cond("missing", driver.OpIsNull, driver.Null()), true},
		{cond("age", driver.OpIsNull, driver.Null()), false},
		{cond("age", driver.OpNotNull, driver.Null()), true},
		{cond("missing", driver.OpEq, driver.Int(1)), false},
		{cond("missing", driver.OpNe, driver.Int(1)), true},
		{cond("_id", driver.OpEq, driver.String("doc-12")), true},
		{cond("_id", driver.OpPrefix, driver.String("doc-")), true},
		{cond("_id", driver.OpContains, driver.String("c-1")), true},
	} {
		got, err := matchAll([]driver.Condition{tc.c}, r)
		if err != nil {
			t.Errorf("%+v: %v", tc.c, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%+v = %v, want %v", tc.c, got, tc.want)
		}
	}
	// Conditions are ANDed.
	if ok, _ := matchAll([]driver.Condition{cond("age", driver.OpEq, driver.Int(30)), cond("active", driver.OpEq, driver.Bool(false))}, r); ok {
		t.Error("AND failed")
	}
	// contains on a number, an unknown op and a non-list in are refused.
	for _, c := range []driver.Condition{
		cond("age", driver.OpContains, driver.String("3")),
		cond("age", driver.OpPrefix, driver.String("3")),
		cond("age", "regex", driver.String("3")),
		cond("age", driver.OpIn, driver.Int(3)),
		cond("tags", driver.OpLt, driver.String("x")),
	} {
		if _, err := matchAll([]driver.Condition{c}, r); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%+v: %v, want ErrUnsupported", c, err)
		}
	}
}

func TestPineconeFilter(t *testing.T) {
	cond := func(f string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: f, Op: op, Value: v}
	}
	f, err := pineconeFilter(driver.Filter{Conditions: []driver.Condition{
		cond("genre", driver.OpEq, driver.String("drama")),
		cond("year", driver.OpGe, driver.Int(2020)),
		cond("year", driver.OpLt, driver.Decimal("2024.5")),
		cond("tags", driver.OpIn, driver.List(driver.String("a"), driver.String("b"))),
		cond("draft", driver.OpIsNull, driver.Null()),
		cond("ok", driver.OpNe, driver.Bool(false)),
		cond("title", driver.OpNotNull, driver.Null()),
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"$and":[{"genre":{"$eq":"drama"}},{"year":{"$gte":2020}},{"year":{"$lt":2024.5}},{"tags":{"$in":["a","b"]}},{"draft":{"$exists":false}},{"ok":{"$ne":false}},{"title":{"$exists":true}}]}`
	if got := jsonOf(t, f); got != want {
		t.Errorf("filter =\n%s\nwant\n%s", got, want)
	}
	if f, err := pineconeFilter(driver.Filter{}); f != nil || err != nil {
		t.Errorf("empty filter = %v, %v", f, err)
	}
	f, err = pineconeFilter(driver.Filter{Raw: ` {"genre":{"$in":["a"]}} `})
	if err != nil || jsonOf(t, f) != `{"genre":{"$in":["a"]}}` {
		t.Errorf("raw filter = %v, %v", f, err)
	}
	for _, bad := range []driver.Filter{
		{Raw: `[1]`},
		{Raw: `{"a":1} x`},
		{Raw: `{"a":1}`, Conditions: []driver.Condition{cond("b", driver.OpEq, driver.Int(1))}},
		{Conditions: []driver.Condition{cond("_id", driver.OpEq, driver.String("x"))}},
		{Conditions: []driver.Condition{cond("name", driver.OpContains, driver.String("x"))}},
		{Conditions: []driver.Condition{cond("name", driver.OpPrefix, driver.String("x"))}},
		{Conditions: []driver.Condition{cond("year", driver.OpLt, driver.String("x"))}},
		{Conditions: []driver.Condition{cond("o", driver.OpEq, driver.ObjectValue())}},
	} {
		if _, err := pineconeFilter(bad); !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("pineconeFilter(%+v) = %v, want a refusal", bad, err)
		}
	}
}

func TestCompareForSort(t *testing.T) {
	for _, tc := range []struct {
		a, b driver.Value
		want int
	}{
		{driver.Int(1), driver.Int(2), -1},
		{driver.Int(2), driver.Float(1.5), 1},
		{driver.String("a"), driver.String("b"), -1},
		{driver.Bool(false), driver.Bool(true), -1},
		{driver.Int(3), driver.Int(3), 0},
		// Mixed kinds order by kind: bool < number < string.
		{driver.String("1"), driver.Int(9), 1},
	} {
		if got := compareSort(tc.a, tc.b); got != tc.want {
			t.Errorf("compareSort(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestPineconeFilterRenamedColumn(t *testing.T) {
	f, err := pineconeFilter(driver.Filter{Conditions: []driver.Condition{{Field: "meta_values", Op: driver.OpEq, Value: driver.String("v")}}})
	if err != nil || jsonOf(t, f) != `{"$and":[{"values":{"$eq":"v"}}]}` {
		t.Errorf("filter = %v, %v", f, err)
	}
}
