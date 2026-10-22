package cosmos

import (
	"testing"

	"github.com/xavidop/tobira/driver"
)

func mustDocs(t *testing.T, raws ...string) []driver.Value {
	t.Helper()
	out := make([]driver.Value, len(raws))
	for i, r := range raws {
		v, err := decodeValue([]byte(r))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = v
	}
	return out
}

func TestInferColumns(t *testing.T) {
	docs := mustDocs(t,
		`{"name":"a","id":"1","n":1,"_rid":"x","_ts":5}`,
		`{"id":"2","n":2.5,"extra":true,"name":null}`,
	)
	cols := inferColumns(docs)
	var names []string
	for _, c := range cols {
		names = append(names, c.Name+":"+c.Type.String())
	}
	want := []string{"id:string", "name:string", "n:float", "extra:bool", "_rid:string", "_ts:int"} // id first, system properties last
	if len(names) != len(want) {
		t.Fatalf("columns %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("columns %v, want %v", names, want)
			break
		}
	}
	scalars := inferColumns(mustDocs(t, `12`, `13`))
	if len(scalars) != 1 || scalars[0].Name != valueColumn || scalars[0].Type != driver.TypeInt {
		t.Errorf("columns of a SELECT VALUE result = %+v, want one %q int column", scalars, valueColumn)
	}
	mixed := inferColumns(mustDocs(t, `"x"`, `1`))
	if len(mixed) != 1 || mixed[0].Type != driver.TypeUnknown || mixed[0].NativeType != "number|string" {
		t.Errorf("mixed scalar column = %+v", mixed)
	}
}

func TestInferFields(t *testing.T) {
	docs := mustDocs(t,
		`{"id":"1","age":30,"tags":["a"],"addr":{"city":"x"},"_etag":"e","_ts":1}`,
		`{"id":"2","age":null,"tags":[],"addr":{"zip":1},"v":[{"k":1}]}`,
	)
	var objs []driver.Object
	for _, d := range docs {
		o, _ := d.AsObject()
		objs = append(objs, o)
	}
	fields := inferFields(objs)
	byName := map[string]driver.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if f := byName["id"]; f.Type != driver.TypeString || f.Nullable {
		t.Errorf("id = %+v", f)
	}
	if f := byName["age"]; f.Type != driver.TypeInt || !f.Nullable || f.NativeType != "number" {
		t.Errorf("age = %+v", f)
	}
	if f := byName["tags"]; f.Type != driver.TypeList || f.NativeType != "array" {
		t.Errorf("tags = %+v", f)
	}
	if f := byName["addr"]; f.Type != driver.TypeObject || len(f.Fields) != 2 {
		t.Errorf("addr = %+v, want two nested fields", f)
	}
	if f := byName["v"]; len(f.Fields) != 1 || f.Fields[0].Name != "k" {
		t.Errorf("v = %+v, want the fields of its element objects", f)
	}
	if f := byName["_etag"]; !f.Generated || !f.Nullable {
		t.Errorf("_etag = %+v, want a generated system property", f)
	}
	if f := byName["_ts"]; !f.Generated {
		t.Errorf("_ts = %+v, want a generated system property", f)
	}
	if fields[0].Name != "id" {
		t.Errorf("first field %q, want id", fields[0].Name)
	}
}
