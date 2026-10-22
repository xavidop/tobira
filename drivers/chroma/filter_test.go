package chroma

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func cond(field string, op driver.Op, v driver.Value) driver.Condition {
	return driver.Condition{Field: field, Op: op, Value: v}
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBuildWhere(t *testing.T) {
	for _, tt := range []struct {
		name    string
		f       driver.Filter
		where   string
		doc     string
		ids     []string
		idsOnly bool
	}{
		{name: "none"},
		{name: "eq bool", f: driver.Filter{Conditions: []driver.Condition{cond("active", driver.OpEq, driver.Bool(true))}}, where: `{"active":{"$eq":true}}`},
		{name: "ne", f: driver.Filter{Conditions: []driver.Condition{cond("name", driver.OpNe, driver.String("x"))}}, where: `{"name":{"$ne":"x"}}`},
		{name: "ranges", f: driver.Filter{Conditions: []driver.Condition{
			cond("age", driver.OpGt, driver.Int(3)), cond("age", driver.OpLe, driver.Int(9)),
			cond("score", driver.OpLt, driver.Float(2.5)), cond("score", driver.OpGe, driver.Decimal("1.5")),
		}}, where: `{"$and":[{"age":{"$gt":3}},{"age":{"$lte":9}},{"score":{"$lt":2.5}},{"score":{"$gte":1.5}}]}`},
		{name: "in", f: driver.Filter{Conditions: []driver.Condition{cond("n", driver.OpIn, driver.List(driver.Int(1), driver.Int(2)))}}, where: `{"n":{"$in":[1,2]}}`},
		{name: "renamed column", f: driver.Filter{Conditions: []driver.Condition{cond("meta_id", driver.OpEq, driver.String("x"))}}, where: `{"id":{"$eq":"x"}}`},
		{name: "id eq", f: driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpEq, driver.Int(7))}}, ids: []string{"7"}, idsOnly: true},
		{name: "id in", f: driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpIn, driver.List(driver.Int(1), driver.String("b")))}}, ids: []string{"1", "b"}, idsOnly: true},
		{name: "id twice intersects", f: driver.Filter{Conditions: []driver.Condition{
			cond("id", driver.OpIn, driver.List(driver.Int(1), driver.Int(2))), cond("id", driver.OpEq, driver.Int(2)),
		}}, ids: []string{"2"}, idsOnly: true},
		{name: "document contains", f: driver.Filter{Conditions: []driver.Condition{cond("document", driver.OpContains, driver.String("db"))}}, doc: `{"$contains":"db"}`},
		{name: "document prefix", f: driver.Filter{Conditions: []driver.Condition{cond("document", driver.OpPrefix, driver.String("a.b"))}}, doc: `{"$regex":"^a\\.b"}`},
		{name: "document twice", f: driver.Filter{Conditions: []driver.Condition{
			cond("document", driver.OpContains, driver.String("a")), cond("document", driver.OpContains, driver.String("b")),
		}}, doc: `{"$and":[{"$contains":"a"},{"$contains":"b"}]}`},
		{name: "raw", f: driver.Filter{Raw: `{"tags":{"$contains":"x"}}`}, where: `{"tags":{"$contains":"x"}}`},
		{name: "raw and condition", f: driver.Filter{Raw: ` {"a":1} `, Conditions: []driver.Condition{cond("b", driver.OpEq, driver.Int(2))}}, where: `{"$and":[{"b":{"$eq":2}},{"a":1}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, err := buildWhere(tt.f)
			if err != nil {
				t.Fatal(err)
			}
			if got := asJSON(t, w.where); got != tt.where {
				t.Errorf("where = %s, want %s", got, tt.where)
			}
			if got := asJSON(t, w.whereDocument); got != tt.doc {
				t.Errorf("where_document = %s, want %s", got, tt.doc)
			}
			if w.idsSet != tt.idsOnly || !slices.Equal(w.ids, tt.ids) {
				t.Errorf("ids = %v (set %v), want %v", w.ids, w.idsSet, tt.ids)
			}
		})
	}
}

func TestBuildWhereRefusals(t *testing.T) {
	for _, f := range []driver.Filter{
		{Conditions: []driver.Condition{cond("id", driver.OpContains, driver.String("1"))}},
		{Conditions: []driver.Condition{cond("id", driver.OpNe, driver.Int(1))}},
		{Conditions: []driver.Condition{cond("id", driver.OpGt, driver.Int(1))}},
		{Conditions: []driver.Condition{cond("name", driver.OpContains, driver.String("a"))}},
		{Conditions: []driver.Condition{cond("name", driver.OpPrefix, driver.String("a"))}},
		{Conditions: []driver.Condition{cond("age", driver.OpIsNull, driver.Null())}},
		{Conditions: []driver.Condition{cond("age", driver.OpNotNull, driver.Null())}},
		{Conditions: []driver.Condition{cond("age", driver.OpEq, driver.Null())}},
		{Conditions: []driver.Condition{cond("document", driver.OpEq, driver.String("a"))}},
		{Conditions: []driver.Condition{cond("document", driver.OpContains, driver.Int(1))}},
		{Conditions: []driver.Condition{cond("embedding", driver.OpEq, driver.Int(1))}},
		{Conditions: []driver.Condition{cond("n", driver.OpIn, driver.Int(1))}},
		{Conditions: []driver.Condition{cond("n", driver.Op("regex"), driver.Int(1))}},
		{Conditions: []driver.Condition{cond("", driver.OpEq, driver.Int(1))}},
		{Conditions: []driver.Condition{cond("id", driver.OpIn, driver.List(manyIDs(maxIDs+1)...))}},
		{Raw: `[1]`},
		{Raw: `{"a":1} x`},
		{Raw: `not json`},
	} {
		if _, err := buildWhere(f); !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("buildWhere(%+v): want ErrUnsupported or ErrSyntax, got %v", f, err)
		}
	}
}

func manyIDs(n int) []driver.Value {
	out := make([]driver.Value, n)
	for i := range out {
		out[i] = driver.Int(int64(i))
	}
	return out
}

func TestBuildWhereIDCap(t *testing.T) {
	if _, err := buildWhere(driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpIn, driver.List(manyIDs(maxIDs)...))}}); err != nil {
		t.Errorf("%d ids: %v", maxIDs, err)
	}
	_, err := buildWhere(driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpIn, driver.List(manyIDs(maxIDs+1)...))}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("%d ids: want ErrUnsupported, got %v", maxIDs+1, err)
	}
}
