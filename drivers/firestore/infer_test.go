package firestore

import (
	"testing"

	"github.com/xavidop/tobira/driver"
)

func obj(ms ...driver.Member) driver.Object { return driver.Object(ms) }

func mem(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }

func TestInferColumns(t *testing.T) {
	rows := []docRow{
		{id: "a", path: "c/a", fields: obj(mem("n", driver.Int(1)), mem("p", driver.Int(2)), mem("s", driver.String("x")))},
		{id: "b", path: "c/b", fields: obj(mem("n", driver.Null()), mem("p", driver.Float(2.5)), mem("s", driver.Int(1)), mem("late", driver.Bool(true)))},
	}
	cols := inferColumns(rows, false)
	want := []struct {
		name   string
		typ    driver.Type
		native string
	}{
		{"_id", driver.TypeString, "document id"},
		{"n", driver.TypeInt, "integer"},
		{"p", driver.TypeFloat, "double|integer"}, // a JS client stores whole numbers as integers
		{"s", driver.TypeUnknown, "integer|string"},
		{"late", driver.TypeBool, "boolean"},
	}
	if len(cols) != len(want) {
		t.Fatalf("cols = %+v", cols)
	}
	for i, w := range want {
		if cols[i].Name != w.name || cols[i].Type != w.typ || cols[i].NativeType != w.native {
			t.Errorf("col %d = %+v, want %+v", i, cols[i], w)
		}
	}
	if !cols[0].Key {
		t.Error("_id is not the key")
	}
	group := inferColumns(rows, true)
	if group[1].Name != "_path" || group[1].Type != driver.TypeString {
		t.Errorf("group cols = %+v", group)
	}
	index := columnIndex(cols)
	row, omitted := rowOf(rows[1], index, len(cols), false)
	if omitted != 0 || row[0].String() != "b" || !row[1].IsNull() || row[4].String() != "true" {
		t.Errorf("row = %v omitted %d", row, omitted)
	}
	narrow := columnIndex(cols[:2])
	if _, omitted := rowOf(rows[1], narrow, 2, false); omitted != 3 {
		t.Errorf("omitted = %d, want 3", omitted)
	}
}

func TestInferFields(t *testing.T) {
	rows := []docRow{
		{fields: obj(mem("addr", driver.ObjectValue(mem("city", driver.String("L")))), mem("tags", driver.List(driver.String("a"))))},
		{fields: obj(mem("items", driver.List(driver.ObjectValue(mem("sku", driver.String("x")))))), natives: []string{"array"}},
		{fields: obj(mem("ref", driver.String("a/b"))), natives: []string{"reference"}},
	}
	fields := inferFields(rows)
	if len(fields) != 5 || fields[4].NativeType != "reference" || fields[0].Name != "_id" || fields[0].Nullable {
		t.Fatalf("fields = %+v", fields)
	}
	addr := fields[1]
	if addr.Name != "addr" || addr.Type != driver.TypeObject || !addr.Nullable || len(addr.Fields) != 1 || addr.Fields[0].Name != "city" {
		t.Errorf("addr = %+v", addr)
	}
	items := fields[3]
	if items.Type != driver.TypeList || len(items.Fields) != 1 || items.Fields[0].Name != "sku" {
		t.Errorf("items = %+v", items)
	}
}

// A stored field named _id would hide behind the document id: it is shown
// as _id_field, and the result says so.
func TestStoredIDField(t *testing.T) {
	m := map[string]any{"_id": "stored", "n": int64(1)}
	d := rowFromMap("doc", "c/doc", m)
	if !d.renamed || len(d.fields) != 2 || d.fields[0].Key != idFieldColumn || d.fields[0].Value.String() != "stored" {
		t.Fatalf("row = %+v", d)
	}
	cols := inferColumns([]docRow{d}, false)
	row, omitted := rowOf(d, columnIndex(cols), len(cols), false)
	if omitted != 0 || row[0].String() != "doc" || row[col0(cols, idFieldColumn)].String() != "stored" {
		t.Errorf("row = %v cols %+v", row, cols)
	}
	if storedName(idFieldColumn) != "_id" || storedName("n") != "n" {
		t.Error("storedName")
	}
}

func col0(cols []driver.Column, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}
