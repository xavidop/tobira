package neo4j

import (
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestQuote(t *testing.T) {
	if got := quote("a`b"); got != "`a``b`" {
		t.Fatalf("quote = %s", got)
	}
}

func TestCondition(t *testing.T) {
	var args []any
	bind := func(v any) string { args = append(args, v); return "$p" }
	for _, tc := range []struct {
		c    driver.Condition
		want string
	}{
		{driver.Condition{Field: "age", Op: driver.OpEq, Value: driver.Int(1)}, "n.`age` = $p"},
		{driver.Condition{Field: "age", Op: driver.OpNe, Value: driver.Int(1)}, "n.`age` <> $p"},
		{driver.Condition{Field: "age", Op: driver.OpLt, Value: driver.Int(1)}, "n.`age` < $p"},
		{driver.Condition{Field: "age", Op: driver.OpLe, Value: driver.Int(1)}, "n.`age` <= $p"},
		{driver.Condition{Field: "age", Op: driver.OpGt, Value: driver.Int(1)}, "n.`age` > $p"},
		{driver.Condition{Field: "age", Op: driver.OpGe, Value: driver.Int(1)}, "n.`age` >= $p"},
		{driver.Condition{Field: "age", Op: driver.OpIn, Value: driver.List(driver.Int(1))}, "n.`age` IN $p"},
		{driver.Condition{Field: "id", Op: driver.OpContains, Value: driver.String("1")}, "toString(n.`id`) CONTAINS toString($p)"},
		{driver.Condition{Field: "name", Op: driver.OpPrefix, Value: driver.String("A")}, "toString(n.`name`) STARTS WITH toString($p)"},
		{driver.Condition{Field: "age", Op: driver.OpIsNull}, "n.`age` IS NULL"},
		{driver.Condition{Field: "age", Op: driver.OpNotNull}, "n.`age` IS NOT NULL"},
		{driver.Condition{Field: idColumn, Op: driver.OpEq, Value: driver.String("4:x:1")}, "elementId(n) = $p"},
	} {
		got, err := condition("n", tc.c, bind)
		if err != nil || got != tc.want {
			t.Errorf("%+v: %q, %v; want %q", tc.c, got, err, tc.want)
		}
	}
	if _, err := condition("n", driver.Condition{Field: "x", Op: driver.OpIn, Value: driver.Int(1)}, bind); err == nil {
		t.Error("in with a scalar was accepted")
	}
	if _, err := condition("n", driver.Condition{Field: "x", Op: "~", Value: driver.Int(1)}, bind); err == nil {
		t.Error("an unknown operator was accepted")
	}
}

func TestParams(t *testing.T) {
	ps, err := params([]driver.Value{driver.Int(7), driver.String("x")})
	if err != nil || len(ps) != 2 || ps["1"] != int64(7) || ps["2"] != "x" {
		t.Fatalf("params = %v, %v", ps, err)
	}
	if ps, err := params(nil); err != nil || ps != nil {
		t.Fatalf("no params = %v, %v", ps, err)
	}
}

func TestCursor(t *testing.T) {
	c := browseCursor{Offset: 40, Tag: 123}
	got, err := decodeCursor(c.encode())
	if err != nil || got != c {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if _, err := decodeCursor("not base64!"); err == nil {
		t.Error("garbage decoded")
	}
	if _, err := decodeCursor(browseCursor{Offset: -1}.encode()); err == nil {
		t.Error("a negative offset decoded")
	}
	if cursorTag("a", map[string]any{"p0": 1}) == cursorTag("a", map[string]any{"p0": 2}) {
		t.Error("the tag ignores the arguments")
	}
	if cursorTag("a", map[string]any{"skip": 1, "limit": 2}) != cursorTag("a", map[string]any{"skip": 9, "limit": 9}) {
		t.Error("the tag depends on skip and limit")
	}
}

func TestSchemaCacheExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newSchemaCache()
	s.now = func() time.Time { return now }
	ref := driver.CollectionRef{Namespace: "neo4j", Name: "Person"}
	s.put(ref, &driver.CollectionSchema{Ref: ref})
	if _, ok := s.get(ref); !ok {
		t.Fatal("a fresh entry was not found")
	}
	now = now.Add(schemaTTL + time.Second)
	if _, ok := s.get(ref); ok {
		t.Fatal("an expired entry was found")
	}
}

func TestStripPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"EXPLAIN MATCH (n) RETURN n":  "MATCH (n) RETURN n",
		"profile\nMATCH (n) RETURN n": "MATCH (n) RETURN n",
		"MATCH (n) RETURN n":          "MATCH (n) RETURN n",
		"EXPLAINED":                   "EXPLAINED",
	} {
		if got := stripPrefix(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestRenderPlan(t *testing.T) {
	root := &driver.PlanNode{Label: "ProduceResults", Detail: "(n)", Rows: 10, Children: []*driver.PlanNode{
		{Label: "NodeByLabelScan", Detail: "n:Person (n)", Rows: 10, Cost: 11},
	}}
	var sb strings.Builder
	renderPlan(&sb, root, 0)
	want := "ProduceResults (n) [rows=10]\n  NodeByLabelScan n:Person (n) [rows=10 db hits=11]\n"
	if sb.String() != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", sb.String(), want)
	}
}
