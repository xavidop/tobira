package weaviate

import (
	"errors"
	"testing"
	"time"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/xavidop/tobira/driver"
)

func peopleSchema() *collSchema {
	return schemaFromClass(&models.Class{Class: "People", VectorIndexType: "hnsw", Properties: []*models.Property{
		{Name: "name", DataType: []string{"text"}},
		{Name: "age", DataType: []string{"int"}},
		{Name: "score", DataType: []string{"number"}},
		{Name: "active", DataType: []string{"boolean"}},
		{Name: "created_at", DataType: []string{"date"}},
		{Name: "tags", DataType: []string{"text[]"}},
	}})
}

func TestBuildWhere(t *testing.T) {
	s := peopleSchema()
	c := func(f string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: f, Op: op, Value: v}
	}
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tt := range []struct {
		conds []driver.Condition
		want  string
	}{
		{nil, ""},
		{[]driver.Condition{c("active", driver.OpEq, driver.Bool(true))}, `{path: ["active"], operator: Equal, valueBoolean: true}`},
		{[]driver.Condition{c("age", driver.OpGe, driver.Int(30))}, `{path: ["age"], operator: GreaterThanEqual, valueInt: 30}`},
		{[]driver.Condition{c("score", driver.OpLt, driver.Decimal("1.5"))}, `{path: ["score"], operator: LessThan, valueNumber: 1.5}`},
		{[]driver.Condition{c("created_at", driver.OpGt, driver.Time(when))}, `{path: ["created_at"], operator: GreaterThan, valueDate: "2026-01-02T03:04:05Z"}`},
		{[]driver.Condition{c("name", driver.OpNe, driver.String(`a"b`))}, `{path: ["name"], operator: NotEqual, valueText: "a\"b"}`},
		{[]driver.Condition{c("name", driver.OpContains, driver.String("da"))}, `{path: ["name"], operator: Like, valueText: "*da*"}`},
		{[]driver.Condition{c("name", driver.OpPrefix, driver.String("Ad"))}, `{path: ["name"], operator: Like, valueText: "Ad*"}`},
		{[]driver.Condition{c("age", driver.OpIsNull, driver.Null())}, `{path: ["age"], operator: IsNull, valueBoolean: true}`},
		{[]driver.Condition{c("age", driver.OpNotNull, driver.Null())}, `{path: ["age"], operator: IsNull, valueBoolean: false}`},
		{[]driver.Condition{c("tags", driver.OpContains, driver.String("beta"))}, `{path: ["tags"], operator: ContainsAny, valueText: ["beta"]}`},
		{[]driver.Condition{c("tags", driver.OpEq, driver.String("beta"))}, `{path: ["tags"], operator: ContainsAny, valueText: ["beta"]}`},
		{[]driver.Condition{c("id", driver.OpEq, driver.Int(7))}, `{path: ["id"], operator: Equal, valueText: "00000000-0000-0000-0000-000000000007"}`},
		{[]driver.Condition{c("age", driver.OpIn, driver.List(driver.Int(1), driver.Int(2)))}, `{operator: Or, operands: [{path: ["age"], operator: Equal, valueInt: 1}, {path: ["age"], operator: Equal, valueInt: 2}]}`},
		{[]driver.Condition{c("age", driver.OpGt, driver.Int(1)), c("active", driver.OpEq, driver.Bool(false))},
			`{operator: And, operands: [{path: ["age"], operator: GreaterThan, valueInt: 1}, {path: ["active"], operator: Equal, valueBoolean: false}]}`},
	} {
		got, none, err := buildWhere(driver.Filter{Conditions: tt.conds}, s)
		if err != nil || none {
			t.Fatalf("buildWhere(%v) = %q, none %v, err %v", tt.conds, got, none, err)
		}
		if got != tt.want {
			t.Errorf("buildWhere(%v):\n got %s\nwant %s", tt.conds, got, tt.want)
		}
	}
	if _, none, err := buildWhere(driver.Filter{Conditions: []driver.Condition{c("age", driver.OpIn, driver.List())}}, s); err != nil || !none {
		t.Errorf("empty in: none %v err %v", none, err)
	}
	raw, _, err := buildWhere(driver.Filter{Raw: `{"path":["age"],"operator":"Equal","valueInt":3}`}, s)
	if err != nil || raw != `{path: ["age"], operator: Equal, valueInt: 3}` {
		t.Errorf("raw = %q, %v", raw, err)
	}
}

func TestBuildWhereRefusals(t *testing.T) {
	s := peopleSchema()
	for _, tt := range []struct {
		cond driver.Condition
		kind error
	}{
		{driver.Condition{Field: "nope", Op: driver.OpEq, Value: driver.Int(1)}, driver.ErrNotFound},
		{driver.Condition{Field: "id", Op: driver.OpContains, Value: driver.String("1")}, driver.ErrUnsupported},
		{driver.Condition{Field: "id", Op: driver.OpGt, Value: driver.Int(1)}, driver.ErrUnsupported},
		{driver.Condition{Field: "vector", Op: driver.OpEq, Value: driver.Int(1)}, driver.ErrUnsupported},
		{driver.Condition{Field: "age", Op: driver.OpContains, Value: driver.String("1")}, driver.ErrUnsupported},
		{driver.Condition{Field: "name", Op: driver.OpContains, Value: driver.String("a*")}, driver.ErrUnsupported},
		{driver.Condition{Field: "age", Op: driver.OpEq, Value: driver.String("x")}, driver.ErrSyntax},
		{driver.Condition{Field: "age", Op: driver.OpEq, Value: driver.Float(1.5)}, driver.ErrSyntax},
		{driver.Condition{Field: "age", Op: driver.OpIn, Value: driver.Int(1)}, driver.ErrSyntax},
		{driver.Condition{Field: "tags", Op: driver.OpGt, Value: driver.String("a")}, driver.ErrUnsupported},
		{driver.Condition{Field: "age", Op: "regex", Value: driver.Int(1)}, driver.ErrUnsupported},
	} {
		if _, _, err := buildWhere(driver.Filter{Conditions: []driver.Condition{tt.cond}}, s); !errors.Is(err, tt.kind) {
			t.Errorf("buildWhere(%+v) = %v, want %v", tt.cond, err, tt.kind)
		}
	}
	if _, _, err := buildWhere(driver.Filter{Raw: `{"operator":"Equal) {"}`}, s); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad raw: %v", err)
	}
}

func TestBrowseCursor(t *testing.T) {
	for _, c := range []browseCursor{
		{after: "00000000-0000-0000-0000-000000000014"},
		{offset: 40},
	} {
		back, err := decodeCursor(c.encode())
		if err != nil || back != c {
			t.Errorf("round trip %+v -> %q -> %+v, %v", c, c.encode(), back, err)
		}
	}
	if c, err := decodeCursor(""); err != nil || c != (browseCursor{}) {
		t.Errorf("empty cursor = %+v, %v", c, err)
	}
	for _, bad := range []string{"x", "a:", "a:not-a-uuid", "o:-1", "o:x", "o:", "a:00000000-0000-0000-0000-000000000014:x"} {
		if _, err := decodeCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decodeCursor(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}

func TestBrowseSort(t *testing.T) {
	s := peopleSchema()
	got, err := browseSort([]driver.SortKey{{Field: "age", Desc: true}}, s)
	if err != nil || got != `[{path: ["age"], order: desc}, {path: ["_id"], order: asc}]` {
		t.Errorf("sort = %q, %v", got, err)
	}
	got, err = browseSort([]driver.SortKey{{Field: "id", Desc: true}}, s)
	if err != nil || got != `[{path: ["_id"], order: desc}]` {
		t.Errorf("id sort = %q, %v", got, err)
	}
	for _, bad := range []driver.SortKey{{Field: "vector"}, {Field: "tags"}, {Field: "nope"}} {
		if _, err := browseSort([]driver.SortKey{bad}, s); err == nil {
			t.Errorf("browseSort(%s) succeeded", bad.Field)
		}
	}
}

// Decimal integers are normalised (or refused with ErrSyntax) before
// they reach GraphQL or JSON.
func TestDecimalIntegers(t *testing.T) {
	s := peopleSchema()
	got, _, err := buildWhere(driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: driver.OpEq, Value: driver.Decimal("007")}}}, s)
	if err != nil || got != `{path: ["age"], operator: Equal, valueInt: 7}` {
		t.Errorf("007 = %q, %v", got, err)
	}
	for _, bad := range []string{"99999999999999999999", "1e3", "+5", ""} {
		_, _, err := buildWhere(driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: driver.OpEq, Value: driver.Decimal(bad)}}}, s)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("Decimal(%q) filter = %v, want ErrSyntax", bad, err)
		}
		if _, err := encodeProp(propInfo{name: "age", dataType: []string{"int"}}, driver.Decimal(bad), "insert"); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("Decimal(%q) write = %v, want ErrSyntax", bad, err)
		}
	}
	v, err := encodeProp(propInfo{name: "age", dataType: []string{"int"}}, driver.Decimal("-007"), "insert")
	if err != nil || v != int64(-7) {
		t.Errorf("-007 write = %#v, %v", v, err)
	}
}

// Invalid UTF-8 would be replaced by U+FFFD and silently match other text.
func TestInvalidUTF8Refused(t *testing.T) {
	s := peopleSchema()
	bad := "a\xffb"
	for _, op := range []driver.Op{driver.OpEq, driver.OpContains, driver.OpPrefix} {
		_, _, err := buildWhere(driver.Filter{Conditions: []driver.Condition{{Field: "name", Op: op, Value: driver.String(bad)}}}, s)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%s with invalid UTF-8 = %v, want ErrSyntax", op, err)
		}
	}
	if _, err := renderLiteral([]byte("{\"valueText\":\"a\xffb\"}")); err == nil {
		t.Error("renderLiteral accepted invalid UTF-8")
	}
	if _, err := parseQuery("{\"collection\":\"A\",\"where\":{\"path\":[\"name\"],\"operator\":\"Equal\",\"valueText\":\"a\xffb\"}}", false); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("request with invalid UTF-8 = %v", err)
	}
	if _, err := encodeProp(propInfo{name: "name", dataType: []string{"text"}}, driver.String(bad), "insert"); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("write with invalid UTF-8 = %v", err)
	}
}
