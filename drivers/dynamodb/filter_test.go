package dynamodb

import (
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

var peopleMeta = &tableMeta{name: "people", keys: []string{"id"}, keyTypes: map[string]string{"id": "N"}}

func TestBuildFilter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		conds []driver.Condition
		expr  string
		names map[string]string
		vals  int
	}{
		{"none", nil, "", nil, 0},
		{"eq", []driver.Condition{{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)}},
			"#f0 = :v0", map[string]string{"#f0": "active"}, 1},
		{"two ANDed", []driver.Condition{
			{Field: "age", Op: driver.OpGe, Value: driver.Int(30)},
			{Field: "name", Op: driver.OpPrefix, Value: driver.String("A")},
		}, "#f0 >= :v0 AND begins_with(#f1, :v1)", map[string]string{"#f0": "age", "#f1": "name"}, 2},
		{"ne lt le gt", []driver.Condition{
			{Field: "a", Op: driver.OpNe, Value: driver.Int(1)}, {Field: "b", Op: driver.OpLt, Value: driver.Int(1)},
			{Field: "c", Op: driver.OpLe, Value: driver.Int(1)}, {Field: "d", Op: driver.OpGt, Value: driver.Int(1)},
		}, "#f0 <> :v0 AND #f1 < :v1 AND #f2 <= :v2 AND #f3 > :v3", nil, 4},
		{"contains", []driver.Condition{{Field: "name", Op: driver.OpContains, Value: driver.String("li")}},
			"contains(#f0, :v0)", nil, 1},
		{"in", []driver.Condition{{Field: "age", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))}},
			"#f0 IN (:v0, :v1)", nil, 2},
		{"empty in matches nothing", []driver.Condition{{Field: "age", Op: driver.OpIn, Value: driver.List()}},
			"(attribute_exists(#f0) AND attribute_not_exists(#f0))", nil, 0},
		{"is null", []driver.Condition{{Field: "age", Op: driver.OpIsNull}},
			"(attribute_not_exists(#f0) OR attribute_type(#f0, :v0))", nil, 1},
		{"not null", []driver.Condition{{Field: "age", Op: driver.OpNotNull}},
			"(attribute_exists(#f0) AND NOT attribute_type(#f0, :v0))", nil, 1},
		{"eq null is is_null", []driver.Condition{{Field: "age", Op: driver.OpEq, Value: driver.Null()}},
			"(attribute_not_exists(#f0) OR attribute_type(#f0, :v0))", nil, 1},
		{"ne null is not_null", []driver.Condition{{Field: "age", Op: driver.OpNe, Value: driver.Null()}},
			"(attribute_exists(#f0) AND NOT attribute_type(#f0, :v0))", nil, 1},
		{"same field twice", []driver.Condition{
			{Field: "age", Op: driver.OpGt, Value: driver.Int(1)}, {Field: "age", Op: driver.OpLt, Value: driver.Int(9)},
		}, "#f0 > :v0 AND #f0 < :v1", map[string]string{"#f0": "age"}, 2},
		{"dotted name is one attribute", []driver.Condition{{Field: "a.b", Op: driver.OpEq, Value: driver.Int(1)}},
			"#f0 = :v0", map[string]string{"#f0": "a.b"}, 1},
	} {
		f, err := buildFilter(tc.conds, peopleMeta)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if f.expr != tc.expr {
			t.Errorf("%s: expr %q, want %q", tc.name, f.expr, tc.expr)
		}
		for k, v := range tc.names {
			if f.names[k] != v {
				t.Errorf("%s: names %v, want %v", tc.name, f.names, tc.names)
			}
		}
		if len(f.values) != tc.vals {
			t.Errorf("%s: %d values, want %d", tc.name, len(f.values), tc.vals)
		}
	}
	f, _ := buildFilter([]driver.Condition{{Field: "age", Op: driver.OpIsNull}}, peopleMeta)
	if s, ok := f.values[":v0"].(*types.AttributeValueMemberS); !ok || s.Value != "NULL" {
		t.Errorf("is_null must compare the attribute type with \"NULL\", got %#v", f.values[":v0"])
	}
}

func TestBuildFilterRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    driver.Condition
		kind error
	}{
		{"empty field", driver.Condition{Field: "", Op: driver.OpEq, Value: driver.Int(1)}, driver.ErrSyntax},
		{"unknown op", driver.Condition{Field: "a", Op: "regex", Value: driver.String("x")}, driver.ErrUnsupported},
		{"contains on a number key", driver.Condition{Field: "id", Op: driver.OpContains, Value: driver.String("1")}, driver.ErrUnsupported},
		{"prefix on a number key", driver.Condition{Field: "id", Op: driver.OpPrefix, Value: driver.String("1")}, driver.ErrUnsupported},
		{"in without a list", driver.Condition{Field: "a", Op: driver.OpIn, Value: driver.Int(1)}, driver.ErrSyntax},
		{"in over 100", driver.Condition{Field: "a", Op: driver.OpIn, Value: driver.List(slices.Repeat([]driver.Value{driver.Int(1)}, 101)...)}, driver.ErrUnsupported},
		{"bad value", driver.Condition{Field: "a", Op: driver.OpEq, Value: driver.Unknown("x", nil, "x")}, driver.ErrUnsupported},
	} {
		_, err := buildFilter([]driver.Condition{tc.c}, peopleMeta)
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: want %v, got %v", tc.name, tc.kind, err)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	meta := &tableMeta{name: "t", keys: []string{"pk", "sk"}, keyTypes: map[string]string{"pk": "S", "sk": "N"}}
	key := map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "a/b"}, "sk": &types.AttributeValueMemberN{Value: "12.5"},
	}
	s, err := keysetCursor(key, meta)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := decodeCursor(s, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur.after) != 2 || cur.after["pk"].(*types.AttributeValueMemberS).Value != "a/b" ||
		cur.after["sk"].(*types.AttributeValueMemberN).Value != "12.5" {
		t.Errorf("round trip: %#v", cur.after)
	}
	bin := &tableMeta{name: "t", keys: []string{"k"}, keyTypes: map[string]string{"k": "B"}}
	s, err = keysetCursor(map[string]types.AttributeValue{"k": &types.AttributeValueMemberB{Value: []byte{0, 255}}}, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := decodeCursor(s, bin); err != nil || string(cur.after["k"].(*types.AttributeValueMemberB).Value) != string([]byte{0, 255}) {
		t.Errorf("binary key: %v %#v", err, cur.after)
	}
	if cur, err := decodeCursor("o:40", meta); err != nil || cur.offset != 40 {
		t.Errorf("offset: %v %+v", err, cur)
	}
	if cur, err := decodeCursor("", meta); err != nil || cur.after != nil || cur.offset != 0 {
		t.Errorf("empty: %v %+v", err, cur)
	}
}

func TestCursorRefuses(t *testing.T) {
	meta := &tableMeta{name: "t", keys: []string{"pk"}, keyTypes: map[string]string{"pk": "S"}}
	other, _ := keysetCursor(map[string]types.AttributeValue{"x": &types.AttributeValueMemberS{Value: "1"}},
		&tableMeta{keys: []string{"x"}, keyTypes: map[string]string{"x": "S"}})
	wrongType, _ := keysetCursor(map[string]types.AttributeValue{"pk": &types.AttributeValueMemberN{Value: "1"}},
		&tableMeta{keys: []string{"pk"}, keyTypes: map[string]string{"pk": "N"}})
	for _, s := range []string{"x", "o:-1", "o:x", "k:!!", "k:e30", other, wrongType, "k:" + string(make([]byte, maxCursorLen))} {
		if _, err := decodeCursor(s, meta); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%.40q: want ErrSyntax, got %v", s, err)
		}
	}
}

func TestCompareAV(t *testing.T) {
	n := func(s string) types.AttributeValue { return &types.AttributeValueMemberN{Value: s} }
	s := func(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
	null := &types.AttributeValueMemberNULL{Value: true}
	ordered := []types.AttributeValue{nil, null, &types.AttributeValueMemberBOOL{Value: false}, &types.AttributeValueMemberBOOL{Value: true},
		n("-3"), n("2"), n("10"), n("1E+2"), s("A"), s("a"), s("b"), &types.AttributeValueMemberB{Value: []byte{1}}}
	for i := range ordered {
		for j := range ordered {
			got := compareAV(ordered[i], ordered[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if i <= 1 && j <= 1 {
				want = 0 // missing and NULL sort together
			}
			if sign(got) != want {
				t.Errorf("compare(%d, %d) = %d, want %d", i, j, got, want)
			}
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func TestSortItems(t *testing.T) {
	items := []map[string]types.AttributeValue{
		{"id": &types.AttributeValueMemberN{Value: "1"}, "age": &types.AttributeValueMemberN{Value: "30"}},
		{"id": &types.AttributeValueMemberN{Value: "2"}},
		{"id": &types.AttributeValueMemberN{Value: "3"}, "age": &types.AttributeValueMemberN{Value: "20"}},
		{"id": &types.AttributeValueMemberN{Value: "4"}, "age": &types.AttributeValueMemberN{Value: "30"}},
	}
	ids := func() []string {
		var out []string
		for _, it := range items {
			out = append(out, it["id"].(*types.AttributeValueMemberN).Value)
		}
		return out
	}
	sortItems(items, []driver.SortKey{{Field: "age"}}, peopleMeta)
	if got := ids(); !slices.Equal(got, []string{"2", "3", "1", "4"}) {
		t.Errorf("asc: %v", got)
	}
	sortItems(items, []driver.SortKey{{Field: "age", Desc: true}}, peopleMeta)
	if got := ids(); !slices.Equal(got, []string{"1", "4", "3", "2"}) {
		t.Errorf("desc (ties by key ascending, missing last): %v", got)
	}
}
