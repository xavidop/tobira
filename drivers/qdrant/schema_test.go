package qdrant

import (
	"testing"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

// TestColumnNamesAreUnique: payload keys that clash with a driver column,
// or that look like an escaped one, get distinct columns, and each column
// maps back to its own key.
func TestColumnNamesAreUnique(t *testing.T) {
	s := testSchema()
	keys := []string{"id", "payload.id", "payload.payload.id", "vector", "payload.vector", "_score", "_distance", "name", "payload.name"}
	seen := map[string]string{}
	for _, k := range keys {
		col := s.column(k)
		if other, dup := seen[col]; dup {
			t.Errorf("keys %q and %q share column %q", other, k, col)
		}
		seen[col] = k
		if back := s.payloadKey(col); back != k {
			t.Errorf("payloadKey(column(%q) = %q) = %q", k, col, back)
		}
		if s.reserved(col) {
			t.Errorf("column %q of key %q is a reserved column", col, k)
		}
	}
	// A plain key is its own column.
	if got := s.column("name"); got != "name" {
		t.Errorf("column(name) = %q", got)
	}
}

// TestShapeTypes: a declared type names the column only when the sampled
// values fit it; mixed point ids give a union like Describe's.
func TestShapeTypes(t *testing.T) {
	s := &collSchema{types: map[string]fieldType{"n": typeInt, "price": typeDecimal}, indexes: map[string]pb.PayloadSchemaType{}}
	pts := []point{
		{id: pb.NewIDNum(1), payload: map[string]*pb.Value{"n": pb.NewValueString("x"), "price": pb.NewValueString("1.50")}},
		{id: pb.NewIDUUID("5c56c793-69f3-4fbf-87e6-c4bf54c28c26"), payload: map[string]*pb.Value{"n": pb.NewValueInt(2), "price": pb.NewValueDouble(2.5)}},
	}
	cols := newShape(s, pts, false, "", nil).columns(pts)
	types := map[string]driver.Column{}
	for _, c := range cols {
		types[c.Name] = c
	}
	if c := types["n"]; c.Type != driver.TypeUnknown || c.NativeType != "string|int" {
		t.Errorf("n (int declared, a string stored) = %s %q, want unknown string|int", c.Type, c.NativeType)
	}
	if c := types["price"]; c.Type != driver.TypeDecimal {
		t.Errorf("price = %s %q, want decimal", c.Type, c.NativeType)
	}
	if c := types["id"]; c.Type != driver.TypeUnknown {
		t.Errorf("mixed ids = %s %q, want a union", c.Type, c.NativeType)
	}
	// With no sample the declared type stands.
	empty := newShape(s, nil, false, "", nil).columns(nil)
	for _, c := range empty {
		if c.Name == "n" && c.Type != driver.TypeInt {
			t.Errorf("n without sample = %s", c.Type)
		}
	}
}
