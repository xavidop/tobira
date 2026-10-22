package postgres

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func schemaWith(pk ...string) *driver.CollectionSchema {
	return &driver.CollectionSchema{
		PrimaryKey: pk,
		Fields: []driver.Field{
			{Name: "id", Type: driver.TypeInt},
			{Name: "at", Type: driver.TypeTime},
			{Name: "code", Type: driver.TypeString},
			{Name: "amount", Type: driver.TypeDecimal},
		},
	}
}

func TestKeysetCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
	cur, err := encodeKeysetCursor([]driver.Value{driver.Int(42), driver.Time(at), driver.String("a/b"), driver.Decimal("12.50")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cur, "k:") {
		t.Fatalf("cursor %q lacks the k: prefix", cur)
	}
	got, err := decodeKeysetCursor(cur, schemaWith("id", "at", "code", "amount"))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := got[0].AsInt(); n != 42 {
		t.Errorf("id = %v", got[0])
	}
	if tm, _ := got[1].AsTime(); !tm.Equal(at) {
		t.Errorf("at = %v, want %v", got[1], at)
	}
	if got[2].String() != "a/b" || got[3].String() != "12.50" || got[3].Type() != driver.TypeDecimal {
		t.Errorf("code/amount = %v / %v (%s)", got[2], got[3], got[3].Type())
	}
}

func TestKeysetCursorInvalid(t *testing.T) {
	enc := func(s string) string { return "k:" + base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, cur := range map[string]string{
		"no prefix":   "abc",
		"offset form": "o:5",
		"bad base64":  "k:!!!",
		"bad json":    enc("not json"),
		"not array":   enc(`{"a":1}`),
		"arity":       enc(`[1,2]`),
		"wrong type":  enc(`["x"]`),
		"null key":    enc(`[null]`),
		"too long":    "k:" + strings.Repeat("A", 5000),
		"bad decimal": enc(`["abc"]`),
	} {
		s := schemaWith("id")
		if name == "bad decimal" {
			s = schemaWith("amount")
		}
		if _, err := decodeKeysetCursor(cur, s); err == nil {
			t.Errorf("%s: cursor %.40q accepted", name, cur)
		}
	}
}

func TestCursorTooLong(t *testing.T) {
	b := &browseQuery{op: "browse", schema: schemaWith("id")}
	err := b.keysetWhere("k:"+strings.Repeat("A", maxCursorLen+1), []string{"id"})
	if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("keyset: want ErrSyntax longer than, got %v", err)
	}
	_, err = parseOffsetCursor("o:" + strings.Repeat("1", maxCursorLen+1))
	if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), "too long") {
		t.Errorf("offset: want ErrSyntax too long, got %v", err)
	}
}

func TestParseOffsetCursor(t *testing.T) {
	if n, err := parseOffsetCursor(""); n != 0 || err != nil {
		t.Errorf("empty = %d, %v", n, err)
	}
	if n, err := parseOffsetCursor("o:40"); n != 40 || err != nil {
		t.Errorf("o:40 = %d, %v", n, err)
	}
	for _, cur := range []string{"40", "o:", "o:-1", "o:x", "k:AAAA"} {
		if _, err := parseOffsetCursor(cur); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%q: want ErrSyntax, got %v", cur, err)
		}
	}
}

func TestBrowseField(t *testing.T) {
	b := &browseQuery{op: "browse", schema: &driver.CollectionSchema{
		Ref:    driver.CollectionRef{Name: "t"},
		Fields: []driver.Field{{Name: "Name"}, {Name: "id"}, {Name: "aB"}, {Name: "Ab"}},
	}}
	for in, want := range map[string]string{"Name": `"Name"`, "name": `"Name"`, "NAME": `"Name"`, "id": `"id"`, "aB": `"aB"`} {
		if got, err := b.field(in); err != nil || got != want {
			t.Errorf("field(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"missing", "AB", `id"; drop`} {
		if _, err := b.field(in); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("field(%q): want ErrNotFound, got %v", in, err)
		}
	}
}

func TestBrowseConditions(t *testing.T) {
	cases := []struct {
		cond   driver.Condition
		clause string
		params int
	}{
		{driver.Condition{Op: driver.OpEq, Value: driver.Int(1)}, `"c" = $1`, 1},
		{driver.Condition{Op: driver.OpEq, Value: driver.Null()}, `"c" IS NULL`, 0},
		{driver.Condition{Op: driver.OpNe, Value: driver.Null()}, `"c" IS NOT NULL`, 0},
		{driver.Condition{Op: driver.OpNe, Value: driver.Int(1)}, `"c" <> $1`, 1},
		{driver.Condition{Op: driver.OpGe, Value: driver.Int(1)}, `"c" >= $1`, 1},
		{driver.Condition{Op: driver.OpIsNull}, `"c" IS NULL`, 0},
		{driver.Condition{Op: driver.OpNotNull}, `"c" IS NOT NULL`, 0},
		{driver.Condition{Op: driver.OpIn, Value: driver.List(driver.Int(1))}, `"c" = ANY($1)`, 1},
		{driver.Condition{Op: driver.OpContains, Value: driver.String("a")}, `CAST("c" AS text) LIKE '%' || CAST($1 AS text) || '%' ESCAPE '\'`, 1},
		{driver.Condition{Op: driver.OpPrefix, Value: driver.String("a")}, `CAST("c" AS text) LIKE CAST($1 AS text) || '%' ESCAPE '\'`, 1},
	}
	for _, tc := range cases {
		b := &browseQuery{op: "browse"}
		if err := b.addCondition(`"c"`, tc.cond); err != nil {
			t.Errorf("%s: %v", tc.cond.Op, err)
			continue
		}
		if b.where[0] != tc.clause || len(b.params) != tc.params {
			t.Errorf("%s: got %q with %d params, want %q with %d", tc.cond.Op, b.where[0], len(b.params), tc.clause, tc.params)
		}
	}
	b := &browseQuery{op: "browse"}
	_ = b.addCondition(`"c"`, driver.Condition{Op: driver.OpContains, Value: driver.String(`50%_\`)})
	if got := b.params[0].String(); got != `50\%\_\\` {
		t.Errorf("escaped pattern = %q", got)
	}
	for name, cond := range map[string]driver.Condition{
		"unknown op":  {Op: "regex", Value: driver.String("x")},
		"in non-list": {Op: driver.OpIn, Value: driver.Int(1)},
		"list eq":     {Op: driver.OpEq, Value: driver.List(driver.Int(1))},
	} {
		if err := (&browseQuery{op: "browse"}).addCondition(`"c"`, cond); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := (&browseQuery{op: "browse"}).addCondition(`"c"`, driver.Condition{Op: "regex"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown op: want ErrUnsupported, got %v", err)
	}
}

func TestParsePlan(t *testing.T) {
	doc := `[{"Plan": {"Node Type": "Nested Loop", "Join Type": "Inner", "Total Cost": 12.5, "Plan Rows": 7, "Plans": [
		{"Node Type": "Seq Scan", "Relation Name": "a", "Total Cost": 1.5, "Plan Rows": 3},
		{"Node Type": "Index Scan", "Relation Name": "b", "Index Name": "b_pkey", "Total Cost": 2}]}}]`
	root, err := parsePlan([]driver.Row{{driver.JSON([]byte(doc))}})
	if err != nil {
		t.Fatal(err)
	}
	if root.Label != "Nested Loop" || root.Detail != "Inner" || root.Cost != 12.5 || root.Rows != 7 || len(root.Children) != 2 {
		t.Fatalf("root = %+v", root)
	}
	if c := root.Children[1]; c.Label != "Index Scan" || c.Detail != "b b_pkey" || c.Rows != -1 {
		t.Errorf("child = %+v", c)
	}
	if _, err := parsePlan([]driver.Row{{driver.String("nope")}}); err == nil {
		t.Error("garbage plan accepted")
	}
	if _, err := parsePlan(nil); err == nil {
		t.Error("empty plan accepted")
	}
}
