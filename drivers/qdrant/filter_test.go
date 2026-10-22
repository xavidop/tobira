package qdrant

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func cond(field string, op driver.Op, v driver.Value) driver.Condition {
	return driver.Condition{Field: field, Op: op, Value: v}
}

func TestBuildFilter(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		f    driver.Filter
		want string // fragments of the proto text form, all required
	}{
		{"empty", driver.Filter{}, ""},
		{"eq bool", driver.Filter{Conditions: []driver.Condition{cond("active", driver.OpEq, driver.Bool(true))}}, `key:"active" match:{boolean:true}`},
		{"eq int", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpEq, driver.Int(30))}}, `key:"age" match:{integer:30}`},
		{"eq string", driver.Filter{Conditions: []driver.Condition{cond("name", driver.OpEq, driver.String("Ada"))}}, `key:"name" match:{keyword:"Ada"}`},
		{"eq float", driver.Filter{Conditions: []driver.Condition{cond("x", driver.OpEq, driver.Float(1.5))}}, `range:{gte:1.5 lte:1.5}`},
		{"ne", driver.Filter{Conditions: []driver.Condition{cond("name", driver.OpNe, driver.String("Ada"))}}, `must_not:{field:{key:"name" match:{keyword:"Ada"}}}`},
		{"lt", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpLt, driver.Int(30))}}, `range:{lt:30}`},
		{"le", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpLe, driver.Float(2.5))}}, `range:{lte:2.5}`},
		{"gt", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpGt, driver.Int(30))}}, `range:{gt:30}`},
		{"ge time", driver.Filter{Conditions: []driver.Condition{cond("created_at", driver.OpGe, driver.Time(ts))}}, `datetime_range:{gte:{seconds:1767323045}}`},
		{"in strings", driver.Filter{Conditions: []driver.Condition{cond("name", driver.OpIn, driver.List(driver.String("a"), driver.String("b")))}}, `keywords:{strings:"a" strings:"b"}`},
		{"in ints", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpIn, driver.List(driver.Int(1), driver.Int(2)))}}, `integers:{integers:1 integers:2}`},
		{"is null", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpIsNull, driver.Null())}}, `is_empty:{key:"age"}`},
		{"not null", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpNotNull, driver.Null())}}, `must_not:{is_empty:{key:"age"}}`},
		{"id eq", driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpEq, driver.Int(7))}}, `has_id:{has_id:{num:7}}`},
		{"id in", driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpIn, driver.List(driver.Int(1), driver.Int(2)))}}, `has_id:{has_id:{num:1} has_id:{num:2}}`},
		{"id ne", driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpNe, driver.Int(7))}}, `must_not:{has_id:{has_id:{num:7}}}`},
		{"raw", driver.Filter{Raw: `{"must":[{"field":{"key":"a","match":{"keyword":"x"}}}]}`}, `key:"a" match:{keyword:"x"}`},
		{"raw and conditions", driver.Filter{Raw: `{"must":[{"field":{"key":"a","match":{"keyword":"x"}}}]}`,
			Conditions: []driver.Condition{cond("b", driver.OpEq, driver.Int(1))}}, `key:"b" match:{integer:1}`},
		{"renamed payload key", driver.Filter{Conditions: []driver.Condition{cond("payload.id", driver.OpEq, driver.Int(1))}}, `key:"id" match:{integer:1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildFilter(tt.f, testSchema())
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if got != nil {
					t.Errorf("want nil filter, got %v", got)
				}
				return
			}
			// prototext spacing is randomised: compare without whitespace.
			text := strings.Join(strings.Fields(got.String()), "")
			for _, frag := range strings.Split(tt.want, "|") {
				if !strings.Contains(text, strings.ReplaceAll(frag, " ", "")) {
					t.Errorf("filter %s lacks %s", text, frag)
				}
			}
		})
	}
}

func TestBuildFilterUnsupported(t *testing.T) {
	for _, c := range []driver.Condition{
		cond("id", driver.OpContains, driver.String("1")),
		cond("name", driver.OpContains, driver.String("da")),
		cond("name", driver.OpPrefix, driver.String("A")),
		cond("id", driver.OpLt, driver.Int(3)),
		cond("score", driver.OpEq, driver.Decimal("1.50")),
		cond("tags", driver.OpEq, driver.List(driver.String("a"))),
		cond("age", driver.OpIn, driver.List(driver.Float(1.5))),
		cond("age", driver.OpLt, driver.String("x")),
		cond("age", driver.Op("regex"), driver.String("x")),
	} {
		_, err := buildFilter(driver.Filter{Conditions: []driver.Condition{c}}, testSchema())
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s %s %s: err = %v, want ErrUnsupported", c.Field, c.Op, c.Value, err)
		}
	}
	if _, err := buildFilter(driver.Filter{Raw: `{"must":`}, testSchema()); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad raw: err = %v, want ErrSyntax", err)
	}
	if _, err := buildFilter(driver.Filter{Raw: `{"points":[1]}`}, testSchema()); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("unknown raw field: err = %v, want ErrSyntax", err)
	}
}

// testSchema is a collection with a default vector and a payload key that
// clashes with the id column.
func testSchema() *collSchema {
	return &collSchema{
		vectors: []vectorInfo{{name: "", column: "vector", size: 4}},
		types:   map[string]fieldType{"score": typeDecimal, "tags": typeJSON, "created_at": typeTime},
	}
}
