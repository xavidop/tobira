package milvus

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"

	"github.com/xavidop/tobira/driver"
)

// Filters Milvus can read that the read-only guard must accept.
var safeFilters = []string{
	`id > 10`,
	`age >= 20 and age < 30 or name == "x"`,
	`name like "Ad%"`,
	`id in [1, 2, 3] && !(active == false)`,
	`tags["a"] == 1 or $meta["k"] != 'v'`,
	`json_contains(tags, "beta") and array_length(arr) > 2`,
	`JSON_CONTAINS_ANY(tags, ["a", "b"])`,
	`text_match(body, "database") || phrase_match(body, "vector db", 1)`,
	`age is null or age is not null`,
	`exists $meta["k"] and not (score <= 1.5e3)`,
	`id > {p1} and name == {name}`,
	`price * 2 + 1 > 10 % 3`,
	`st_intersects(geo, "POINT (1 1)")`,
	`random_sample(0.1)`,
	`name == "it's \"quoted\" \\ fine"`,
	`id == 0x1F`,
	``,
}

func TestCheckFilterAcceptsReads(t *testing.T) {
	for _, f := range safeFilters {
		for _, ro := range []bool{true, false} {
			if err := checkFilter(f, ro); err != nil {
				t.Errorf("checkFilter(%q, ro=%v) = %v, want nil", f, ro, err)
			}
		}
	}
}

func TestCheckFilterRefusals(t *testing.T) {
	for _, tc := range []struct {
		filter string
		// rwOK: a writable connection lets the server judge it.
		rwOK bool
	}{
		{`unknownfn(1)`, true},
		{`like(a)`, true},
		{`null(1)`, true},
		{`true(1)`, true},
		{`is (1)`, true},
		{`id > 1 and delete_all()`, true},
		{`json_contains (tags, 1) or evil (x)`, true},
		{`id > 1)`, false},
		{`(id > 1`, false},
		{`id in [1, 2`, false},
		{`id in 1, 2]`, false},
		{`name == "unterminated`, false},
		{`name == 'unterminated`, false},
		{"name == \"line\nbreak\"", false},
		{`id > 1; drop`, false},
		{"id > `1`", false},
		{`id > 1 # comment`, false},
		{`id > {p1`, false},
		{`id > {}`, false},
		{`id > {1x}`, false},
		{`$other["k"] == 1`, false},
		{`id @> 1`, false},
		{`name == "x"` + "\x00", false},
		{strings.Repeat("(", 70000), false},
	} {
		err := checkFilter(tc.filter, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkFilter(%q, ro) = %v, want ErrReadOnly", tc.filter, err)
		}
		err = checkFilter(tc.filter, false)
		if tc.rwOK && err != nil {
			t.Errorf("checkFilter(%q, rw) = %v, want nil (the server judges unknown functions)", tc.filter, err)
		}
		if !tc.rwOK && !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkFilter(%q, rw) = %v, want ErrSyntax", tc.filter, err)
		}
	}
}

func TestBuildFilterUsesTemplates(t *testing.T) {
	s := peopleSchema()
	evil := `x" or id > 0 or name == "`
	params := exprParams{}
	expr, err := buildFilter(driver.Filter{Conditions: []driver.Condition{
		{Field: "name", Op: driver.OpEq, Value: driver.String(evil)},
		{Field: "age", Op: driver.OpGe, Value: driver.Int(20)},
		{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))},
		{Field: "age", Op: driver.OpIsNull},
		{Field: "name", Op: driver.OpNotNull},
	}}, s, true, params)
	if err != nil {
		t.Fatal(err)
	}
	want := `(name == {tobira_f0}) and (age >= {tobira_f1}) and (id in {tobira_f2}) and (age is null) and (name is not null)`
	if expr != want {
		t.Errorf("expr = %s\nwant   %s", expr, want)
	}
	if strings.Contains(expr, "or id") {
		t.Fatal("a value reached the expression text")
	}
	if got := params["tobira_f0"].GetStringVal(); got != evil {
		t.Errorf("param f0 = %q, want the raw value", got)
	}
	if got := params["tobira_f1"].GetInt64Val(); got != 20 {
		t.Errorf("param f1 = %d", got)
	}
	arr := params["tobira_f2"].GetArrayVal().GetLongData().GetData()
	if len(arr) != 2 || arr[0] != 1 || arr[1] != 2 {
		t.Errorf("param f2 = %v", params["tobira_f2"])
	}
	if err := checkFilter(expr, true); err != nil {
		t.Errorf("the built filter fails the guard: %v", err)
	}
}

func TestBuildFilterDynamicKeys(t *testing.T) {
	s := peopleSchema()
	params := exprParams{}
	expr, err := buildFilter(driver.Filter{Conditions: []driver.Condition{
		{Field: "color", Op: driver.OpEq, Value: driver.String("red")},
		{Field: "$meta.embedding", Op: driver.OpGt, Value: driver.Float(1.5)},
		{Field: "color", Op: driver.OpIsNull},
		{Field: "color", Op: driver.OpNotNull},
	}}, s, false, params)
	if err != nil {
		t.Fatal(err)
	}
	want := `($meta["color"] == {tobira_f0}) and ($meta["embedding"] > {tobira_f1}) and (not exists $meta["color"]) and (exists $meta["color"])`
	if expr != want {
		t.Errorf("expr = %s\nwant   %s", expr, want)
	}
	if params["tobira_f1"].GetFloatVal() != 1.5 {
		t.Errorf("param f1 = %v", params["tobira_f1"])
	}
	// Key names are syntax, not values: anything outside the identifier
	// alphabet is refused rather than escaped.
	for _, key := range []string{`a"]==1 or $meta["b`, `a b`, `ключ`, ``, `$meta.`} {
		_, err := buildFilter(driver.Filter{Conditions: []driver.Condition{{Field: key, Op: driver.OpEq, Value: driver.Int(1)}}}, s, false, exprParams{})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("dynamic key %q: %v, want ErrUnsupported", key, err)
		}
	}
}

func TestBuildFilterWithoutDynamicField(t *testing.T) {
	p := peopleProto()
	p.EnableDynamicField = false
	p.Fields = p.Fields[:len(p.Fields)-1]
	s := schemaFromProtoForTest(p)
	_, err := buildFilter(driver.Filter{Conditions: []driver.Condition{{Field: "nope", Op: driver.OpEq, Value: driver.Int(1)}}}, s, false, exprParams{})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown field without a dynamic field: %v, want ErrNotFound", err)
	}
}

func TestBuildFilterLike(t *testing.T) {
	s := peopleSchema()
	params := exprParams{}
	expr, err := buildFilter(driver.Filter{Conditions: []driver.Condition{
		{Field: "name", Op: driver.OpPrefix, Value: driver.String(`x_y`)},
		{Field: "name", Op: driver.OpContains, Value: driver.String(`a"b`)},
	}}, s, false, params)
	if err != nil {
		t.Fatal(err)
	}
	if expr != `(name like {tobira_f0}) and (name like {tobira_f1})` {
		t.Errorf("expr = %s", expr)
	}
	if got := params["tobira_f0"].GetStringVal(); got != `x\_y%` {
		t.Errorf("prefix pattern = %q", got)
	}
	if got := params["tobira_f1"].GetStringVal(); got != `%a"b%` {
		t.Errorf("contains pattern = %q", got)
	}
}

func TestBuildFilterUnsupported(t *testing.T) {
	s := peopleSchema()
	for _, c := range []driver.Condition{
		{Field: "id", Op: driver.OpContains, Value: driver.String("1")},
		{Field: "embedding", Op: driver.OpEq, Value: driver.Vector([]float32{1, 2})},
		{Field: "name", Op: driver.OpEq, Value: driver.ObjectValue()},
		{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.String("x"))},
		{Field: "id", Op: driver.OpIn, Value: driver.Int(1)},
		{Field: "age", Op: driver.OpGt, Value: driver.Float(math.Inf(1))},
		{Field: "age", Op: "regex", Value: driver.String("x")},
		{Field: "name", Op: driver.OpContains, Value: driver.String("50%")},
		{Field: "name", Op: driver.OpPrefix, Value: driver.String(`a\b`)},
	} {
		_, err := buildFilter(driver.Filter{Conditions: []driver.Condition{c}}, s, false, exprParams{})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s %s %s: %v, want ErrUnsupported", c.Field, c.Op, c.Value, err)
		}
	}
}

func TestBuildFilterRaw(t *testing.T) {
	s := peopleSchema()
	expr, err := buildFilter(driver.Filter{Raw: `age > 3 or age < 1`, Conditions: []driver.Condition{{Field: "id", Op: driver.OpEq, Value: driver.Int(1)}}}, s, true, exprParams{})
	if err != nil {
		t.Fatal(err)
	}
	if expr != `(age > 3 or age < 1) and (id == {tobira_f0})` {
		t.Errorf("expr = %s", expr)
	}
	// A raw filter the guard cannot read is refused on a read-only browse.
	_, err = buildFilter(driver.Filter{Raw: `id > 1) or (id < 0`}, s, true, exprParams{})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("unbalanced raw filter (ro): %v, want ErrReadOnly", err)
	}
	_, err = buildFilter(driver.Filter{Raw: `mystery(id)`}, s, true, exprParams{})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("unknown function in a raw filter (ro): %v, want ErrReadOnly", err)
	}
	_, err = buildFilter(driver.Filter{Raw: `id > 1) or (id < 0`}, s, false, exprParams{})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("unbalanced raw filter (rw): %v, want ErrSyntax", err)
	}
}

func TestAndExpr(t *testing.T) {
	if got := andExpr("", "a", "", "b or c"); got != "(a) and (b or c)" {
		t.Errorf("andExpr = %q", got)
	}
	if got := andExpr("", ""); got != "" {
		t.Errorf("andExpr of nothing = %q", got)
	}
}

func schemaFromProtoForTest(p *schemapb.CollectionSchema) *collSchema {
	return schemaFromProtoSchema("default", "people", p)
}
