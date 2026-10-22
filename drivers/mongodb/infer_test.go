package mongodb

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

func docs(t *testing.T, ds ...bson.D) []bson.Raw {
	t.Helper()
	out := make([]bson.Raw, len(ds))
	for i, d := range ds {
		b, err := bson.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

func TestInferColumns(t *testing.T) {
	in := docs(t,
		bson.D{{Key: "name", Value: "a"}, {Key: "_id", Value: int32(1)}, {Key: "age", Value: int32(3)}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "age", Value: "x"}, {Key: "extra", Value: nil}},
		bson.D{{Key: "_id", Value: int64(3)}, {Key: "age", Value: nil}},
	)
	cols := inferColumns(in)
	var names []string
	for _, c := range cols {
		names = append(names, c.Name)
	}
	if want := []string{"_id", "name", "age", "extra"}; !equalStrings(names, want) {
		t.Fatalf("columns %v, want %v (_id first, then first-seen)", names, want)
	}
	if cols[0].Type != driver.TypeInt || cols[0].NativeType != "int|long" {
		t.Errorf("_id column %+v", cols[0])
	}
	if cols[2].Type != driver.TypeUnknown || cols[2].NativeType != "int|string" {
		t.Errorf("age column %+v", cols[2])
	}
	if cols[3].Type != driver.TypeNull {
		t.Errorf("extra (only null) column %+v", cols[3])
	}
	idx := columnIndex(cols)
	row, omitted := rowOf(in[1], idx, len(cols))
	if omitted != 0 || row[0].String() != "2" || !row[1].IsNull() || row[2].String() != "x" {
		t.Errorf("row %v omitted %d", row, omitted)
	}
	late := docs(t, bson.D{{Key: "_id", Value: int32(9)}, {Key: "new", Value: 1}, {Key: "other", Value: 2}})[0]
	if _, omitted := rowOf(late, idx, len(cols)); omitted != 2 {
		t.Errorf("fields outside the columns: omitted = %d, want 2", omitted)
	}
}

func TestInferFields(t *testing.T) {
	in := docs(t,
		bson.D{{Key: "_id", Value: "d1"}, {Key: "address", Value: bson.D{{Key: "city", Value: "London"}}}, {Key: "items", Value: bson.A{bson.D{{Key: "sku", Value: "a"}}}}},
		bson.D{{Key: "_id", Value: "d2"}, {Key: "address", Value: bson.D{{Key: "city", Value: "Paris"}, {Key: "geo", Value: bson.D{{Key: "lat", Value: 1.5}}}}}},
	)
	fields := inferFields(in)
	if len(fields) != 3 || fields[0].Name != "_id" || fields[0].Nullable {
		t.Fatalf("fields %+v", fields)
	}
	addr := fields[1]
	if addr.Type != driver.TypeObject || len(addr.Fields) != 2 || addr.Fields[1].Name != "geo" || addr.Fields[1].Fields[0].Name != "lat" {
		t.Errorf("address %+v", addr)
	}
	if !addr.Fields[1].Nullable || addr.Fields[0].Nullable {
		t.Errorf("geo is missing in one document (nullable), city never: %+v", addr.Fields)
	}
	items := fields[2]
	if items.Type != driver.TypeList || !items.Nullable || len(items.Fields) != 1 || items.Fields[0].Name != "sku" {
		t.Errorf("items %+v", items)
	}
}

// A field holding a document in some samples and an array of documents in
// others merges both shapes into its nested fields.
func TestInferFieldsMergesDocumentAndArrayShapes(t *testing.T) {
	in := docs(t,
		bson.D{{Key: "x", Value: bson.D{{Key: "a", Value: 1}}}},
		bson.D{{Key: "x", Value: bson.A{bson.D{{Key: "b", Value: "s"}}}}},
	)
	f := inferFields(in)[0]
	var names []string
	for _, n := range f.Fields {
		names = append(names, n.Name)
	}
	if !equalStrings(names, []string{"a", "b"}) {
		t.Errorf("nested fields %v, want [a b]", names)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
