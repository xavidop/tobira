package pinecone

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestFromMeta(t *testing.T) {
	list, _ := structpb.NewList([]any{"a", "b"})
	obj, _ := structpb.NewStruct(map[string]any{"z": 1, "a": "x"})
	for _, tc := range []struct {
		in   *structpb.Value
		want driver.Value
	}{
		{structpb.NewNumberValue(1), driver.Int(1)},
		{structpb.NewNumberValue(-42), driver.Int(-42)},
		{structpb.NewNumberValue(1.5), driver.Float(1.5)},
		{structpb.NewNumberValue(1 << 60), driver.Float(1 << 60)},
		{structpb.NewNumberValue(math.Inf(1)), driver.Float(math.Inf(1))},
		{structpb.NewStringValue("s"), driver.String("s")},
		{structpb.NewBoolValue(true), driver.Bool(true)},
		{structpb.NewNullValue(), driver.Null()},
		{nil, driver.Null()},
		{structpb.NewListValue(list), driver.List(driver.String("a"), driver.String("b"))},
		{structpb.NewStructValue(obj), driver.ObjectValue(driver.Member{Key: "a", Value: driver.String("x")}, driver.Member{Key: "z", Value: driver.Int(1)})},
	} {
		if got := fromMeta(tc.in); !drivertest.EqualValue(got, tc.want) || got.Type() != tc.want.Type() {
			t.Errorf("fromMeta(%v) = %s (%s), want %s (%s)", tc.in, got, got.Type(), tc.want, tc.want.Type())
		}
	}
}

func TestToMeta(t *testing.T) {
	when := time.Date(2026, 6, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	ok := []struct {
		in   driver.Value
		want any
	}{
		{driver.String("s"), "s"},
		{driver.Int(7), float64(7)},
		{driver.Float(1.25), 1.25},
		{driver.Decimal("12.50"), 12.5},
		{driver.Bool(false), false},
		{driver.Time(when), "2026-06-01T11:00:00Z"},
		{driver.JSON([]byte(`["admin","beta"]`)), []any{"admin", "beta"}},
		{driver.JSON([]byte(`[]`)), []any{}},
		{driver.JSON([]byte(`"x"`)), "x"},
		{driver.JSON([]byte(`3`)), float64(3)},
		{driver.List(driver.String("a")), []any{"a"}},
		{driver.List(), []any{}},
	}
	for _, tc := range ok {
		got, err := toMeta("f", tc.in)
		if err != nil {
			t.Errorf("toMeta(%s): %v", tc.in, err)
			continue
		}
		want, _ := structpb.NewValue(tc.want)
		if got.String() != want.String() {
			t.Errorf("toMeta(%s) = %v, want %v", tc.in, got, want)
		}
	}
	if v, err := toMeta("f", driver.Null()); v != nil || err != nil {
		t.Errorf("toMeta(NULL) = %v, %v", v, err)
	}
	for _, in := range []driver.Value{
		driver.JSON([]byte(`{"a":1}`)),
		driver.JSON([]byte(`[1,2]`)),
		driver.JSON([]byte(`null`)),
		driver.JSON([]byte(`{bad`)),
		driver.List(driver.Int(1)),
		driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}),
		driver.Vector([]float32{1}),
		driver.Bytes([]byte("x")),
		driver.Float(math.NaN()),
		driver.Float(math.Inf(-1)),
		driver.Int(1 << 60),
		driver.Decimal("1e400"),
		driver.Decimal("0.1000000000000000000001"),
	} {
		if _, err := toMeta("f", in); !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("toMeta(%s %s) = %v, want a refusal", in.Type(), in, err)
		}
	}
}

func TestShape(t *testing.T) {
	recs := []record{
		{id: "b", values: []float32{1, 2}, meta: map[string]driver.Value{"n": driver.Int(1), "name": driver.String("x"), "mix": driver.Int(3)}},
		{id: "a", values: []float32{3, 4}, meta: map[string]driver.Value{"n": driver.Float(1.5), "mix": driver.String("y"), "tags": driver.List(driver.String("t"))}},
	}
	src := &driver.CollectionRef{Namespace: "idx", Name: "ns"}
	sh := newShape(recs, shapeOptions{values: true, source: src})
	cols := sh.columns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	want := []string{"_id", "values", "mix", "n", "name", "tags"}
	if len(names) != len(want) {
		t.Fatalf("columns = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("columns = %v, want %v", names, want)
		}
	}
	types := map[string]driver.Type{"_id": driver.TypeString, "values": driver.TypeVector, "mix": driver.TypeUnknown, "n": driver.TypeFloat, "name": driver.TypeString, "tags": driver.TypeList}
	for _, c := range cols {
		if c.Type != types[c.Name] {
			t.Errorf("column %s type %s, want %s", c.Name, c.Type, types[c.Name])
		}
		if c.Source == nil || *c.Source != *src {
			t.Errorf("column %s source %v", c.Name, c.Source)
		}
	}
	if !cols[0].Key || cols[2].Key {
		t.Error("only _id is the key")
	}
	if cols[1].NativeType != "vector(2)" {
		t.Errorf("values native type %q", cols[1].NativeType)
	}
	row, dropped := sh.row(recs[0])
	if len(dropped) != 0 {
		t.Errorf("dropped %v", dropped)
	}
	// An Int in a Float column becomes a Float; a missing key is NULL.
	if row[3].Type() != driver.TypeFloat || !row[5].IsNull() || row[0] != driver.String("b") {
		t.Errorf("row = %v", row)
	}
	// A key with no column is reported.
	_, dropped = sh.row(record{id: "c", meta: map[string]driver.Value{"late": driver.Int(1)}})
	if len(dropped) != 1 || dropped[0] != "late" {
		t.Errorf("dropped = %v", dropped)
	}
	// A score column comes right after _id, converted.
	sh = newShape([]record{{id: "a", score: 0.25, meta: map[string]driver.Value{}}}, shapeOptions{score: distanceColumn, scoreFn: func(s float32) float64 { return 1 - float64(s) }})
	cols = sh.columns()
	if cols[1].Name != distanceColumn || cols[1].Type != driver.TypeFloat {
		t.Errorf("score column = %+v", cols)
	}
	row, _ = sh.row(record{id: "a", score: 0.25})
	if f, _ := row[1].AsFloat(); f != 0.75 {
		t.Errorf("distance = %v", row[1])
	}
	// Sparse values are an object column.
	sh = newShape([]record{{id: "s", sparse: &pinecone.SparseValues{Indices: []uint32{3}, Values: []float32{0.5}}}}, shapeOptions{values: true})
	cols = sh.columns()
	if len(cols) != 2 || cols[1].Name != sparseColumn || cols[1].Type != driver.TypeObject {
		t.Errorf("sparse columns = %+v", cols)
	}
}

// Metadata keys that collide with the record columns, or that start with
// meta_, are shown with a meta_ prefix, with a warning.
func TestShapeRenamesCollidingKeys(t *testing.T) {
	rec := record{id: "r", values: []float32{1}, meta: map[string]driver.Value{
		"values": driver.String("v"), "_id": driver.String("i"), "_score": driver.Int(1),
		"sparse_values": driver.Bool(true), "_distance": driver.Int(2), "meta_x": driver.Int(3), "plain": driver.Int(4),
	}}
	sh := newShape([]record{rec}, shapeOptions{values: true, score: scoreColumn})
	var names []string
	for _, c := range sh.columns() {
		names = append(names, c.Name)
	}
	want := []string{"_id", "_score", "values", "meta__distance", "meta__id", "meta__score", "meta_meta_x", "meta_sparse_values", "meta_values", "plain"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("columns = %v, want %v", names, want)
	}
	row, dropped := sh.row(rec)
	if len(dropped) != 0 || row[8] != driver.String("v") || row[4] != driver.String("i") || row[6] != driver.Int(3) {
		t.Errorf("row = %v", row)
	}
	w := strings.Join(sh.warnings(), " ")
	if !strings.Contains(w, `"values" is shown as meta_values`) || !strings.Contains(w, `"meta_x" is shown as meta_meta_x`) {
		t.Errorf("warnings = %q", w)
	}
	if newShape([]record{{id: "a", meta: map[string]driver.Value{"plain": driver.Int(1)}}}, shapeOptions{}).warnings() != nil {
		t.Error("no rename, no warning")
	}
	for col, key := range map[string]string{"plain": "plain", "meta_values": "values", "meta_meta_x": "meta_x", "meta__id": "_id"} {
		if got := metaKey(col); got != key {
			t.Errorf("metaKey(%q) = %q, want %q", col, got, key)
		}
		if got := displayName(key); got != col {
			t.Errorf("displayName(%q) = %q, want %q", key, got, col)
		}
	}
	// Filters and sorting address the renamed column.
	if v := fieldValue(rec, "meta_values"); v != driver.String("v") {
		t.Errorf("fieldValue(meta_values) = %v", v)
	}
	if v := fieldValue(rec, idColumn); v != driver.String("r") {
		t.Errorf("fieldValue(_id) = %v", v)
	}
}
