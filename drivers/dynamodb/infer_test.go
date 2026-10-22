package dynamodb

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

func n(s string) types.AttributeValue { return &types.AttributeValueMemberN{Value: s} }
func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func TestInferColumns(t *testing.T) {
	items := []map[string]types.AttributeValue{
		{"zeta": s("z"), "id": n("1"), "score": n("1.5"), "mixed": n("1"), "alpha": &types.AttributeValueMemberNULL{Value: true}},
		{"id": n("2"), "score": n("2"), "mixed": s("x"), "alpha": s("a"), "set": &types.AttributeValueMemberSS{Value: []string{"a"}}},
	}
	cols := inferColumns(items, []string{"id"}, nil)
	var names []string
	for _, c := range cols {
		names = append(names, c.Name)
	}
	if !equalStrings(names, []string{"id", "alpha", "mixed", "score", "set", "zeta"}) {
		t.Fatalf("names %v: the key first, then the rest sorted", names)
	}
	want := map[string]struct {
		typ    driver.Type
		native string
	}{
		"id":    {driver.TypeInt, "N"},
		"alpha": {driver.TypeString, "S"},
		"mixed": {driver.TypeUnknown, "N|S"},
		"score": {driver.TypeDecimal, "N"},
		"set":   {driver.TypeList, "SS"},
		"zeta":  {driver.TypeString, "S"},
	}
	for _, c := range cols {
		w := want[c.Name]
		if c.Type != w.typ || c.NativeType != w.native {
			t.Errorf("%s: type %s native %q, want %s %q", c.Name, c.Type, c.NativeType, w.typ, w.native)
		}
	}
	cols = inferColumns(items, []string{"id"}, []string{"score", "id", "nope"})
	names = nil
	for _, c := range cols {
		names = append(names, c.Name)
	}
	if !equalStrings(names, []string{"score", "id", "nope"}) {
		t.Errorf("a projection keeps its order and only its names: %v", names)
	}
}

func TestInferFields(t *testing.T) {
	items := []map[string]types.AttributeValue{
		{"id": n("1"), "age": n("30"), "doc": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"a": s("x")}}},
		{"id": n("2"), "age": &types.AttributeValueMemberNULL{Value: true}, "doc": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"b": n("1")}}},
		{"id": n("3")},
	}
	meta := &tableMeta{name: "t", keys: []string{"id"}, keyTypes: map[string]string{"id": "N"}}
	fields := inferFields(items, meta)
	byName := map[string]driver.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if fields[0].Name != "id" || fields[0].Nullable || fields[0].Type != driver.TypeInt {
		t.Errorf("id: %+v", fields[0])
	}
	if f := byName["age"]; f.Type != driver.TypeInt || !f.Nullable {
		t.Errorf("age: %+v", f)
	}
	if f := byName["doc"]; f.Type != driver.TypeObject || len(f.Fields) != 2 || f.Fields[0].Name != "a" || !f.Fields[0].Nullable {
		t.Errorf("doc: %+v", f)
	}
	// A key attribute the sample never shows still gets a field from its
	// declared type.
	empty := inferFields(nil, &tableMeta{keys: []string{"pk", "sk"}, keyTypes: map[string]string{"pk": "S", "sk": "N"}})
	if len(empty) != 2 || empty[0].Type != driver.TypeString || empty[1].Type != driver.TypeDecimal || empty[1].NativeType != "N" {
		t.Errorf("key fields without a sample: %+v", empty)
	}
}
