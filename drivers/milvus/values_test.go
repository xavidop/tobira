package milvus

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestCellValue(t *testing.T) {
	ts := "2026-01-02T03:04:05Z"
	nullable, _ := column.NewNullableColumnInt64("n", []int64{5}, []bool{false, true})
	sparse, _ := entity.NewSliceSparseEmbedding([]uint32{1, 7}, []float32{0.5, 2})
	for _, tc := range []struct {
		name string
		col  column.Column
		i    int
		want driver.Value
	}{
		{"bool", column.NewColumnBool("b", []bool{true}), 0, driver.Bool(true)},
		{"int8", column.NewColumnInt8("i", []int8{-3}), 0, driver.Int(-3)},
		{"int32", column.NewColumnInt32("i", []int32{70000}), 0, driver.Int(70000)},
		{"int64", column.NewColumnInt64("i", []int64{math.MaxInt64}), 0, driver.Int(math.MaxInt64)},
		{"float keeps its float32 spelling", column.NewColumnFloat("f", []float32{0.1}), 0, driver.Float(0.1)},
		{"double", column.NewColumnDouble("d", []float64{12.5}), 0, driver.Float(12.5)},
		{"varchar", column.NewColumnVarChar("s", []string{"Ōtani"}), 0, driver.String("Ōtani")},
		{"json", column.NewColumnJSONBytes("j", [][]byte{[]byte(`["a"]`)}), 0, driver.JSON([]byte(`["a"]`))},
		{"timestamptz", column.NewColumnTimestamptzIsoString("t", []string{ts}), 0, driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))},
		{"timestamptz with offset", column.NewColumnTimestamptzIsoString("t", []string{"2026-01-02T05:04:05+02:00"}), 0, driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))},
		{"null", nullable, 0, driver.Null()},
		{"after null", nullable, 1, driver.Int(5)},
		{"array", column.NewColumnVarCharArray("a", [][]string{{"x", "y"}}), 0, driver.List(driver.String("x"), driver.String("y"))},
		{"float vector", column.NewColumnFloatVector("v", 2, [][]float32{{1, 2}}), 0, driver.Vector([]float32{1, 2})},
		{"binary vector", column.NewColumnBinaryVector("v", 16, [][]byte{{0xff, 0x01}}), 0, driver.Bytes([]byte{0xff, 0x01})},
		{"float16 vector", column.NewColumnFloat16VectorFromFp32Vector("v", 2, [][]float32{{1, 0.5}}), 0, driver.Vector([]float32{1, 0.5})},
		{"int8 vector", column.NewColumnInt8Vector("v", 2, [][]int8{{-1, 3}}), 0, driver.List(driver.Int(-1), driver.Int(3))},
		{"sparse", column.NewColumnSparseVectors("v", []entity.SparseEmbedding{sparse}), 0, driver.ObjectValue(
			driver.Member{Key: "indices", Value: driver.List(driver.Int(1), driver.Int(7))},
			driver.Member{Key: "values", Value: driver.Vector([]float32{0.5, 2})})},
		{"geometry", column.NewColumnGeometryWKT("g", []string{"POINT (1 2)"}), 0, driver.String("POINT (1 2)")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cellValue(tc.col, tc.i)
			if !drivertest.EqualValue(tc.want, got) {
				t.Errorf("cellValue = %s (%s), want %s (%s)", got, got.Type(), tc.want, tc.want.Type())
			}
		})
	}
}

func TestDynamicMembers(t *testing.T) {
	ms, err := dynamicMembers([]byte(`{"z":1,"a":2.5,"big":12345678901234567890,"s":"x","o":{"k":[true,null]},"n":null}`))
	if err != nil {
		t.Fatal(err)
	}
	want := driver.Object{
		{Key: "a", Value: driver.Float(2.5)},
		{Key: "big", Value: driver.Float(12345678901234567890)},
		{Key: "n", Value: driver.Null()},
		{Key: "o", Value: driver.ObjectValue(driver.Member{Key: "k", Value: driver.List(driver.Bool(true), driver.Null())})},
		{Key: "s", Value: driver.String("x")},
		{Key: "z", Value: driver.Int(1)},
	}
	if !drivertest.EqualValue(driver.ObjectValue(want...), driver.ObjectValue(ms...)) {
		t.Errorf("dynamicMembers = %v", ms)
	}
	if _, err := dynamicMembers([]byte(`[1]`)); err == nil {
		t.Error("a non-object $meta must fail")
	}
	if ms, err := dynamicMembers(nil); err != nil || len(ms) != 0 {
		t.Errorf("empty $meta = %v, %v", ms, err)
	}
}

func TestDynamicColumnNames(t *testing.T) {
	s := peopleSchema()
	for key, want := range map[string]string{
		"color":       "color",
		"name":        "$meta.name",
		"_score":      "$meta._score",
		"_distance":   "$meta._distance",
		"$meta.x":     "$meta.$meta.x",
		"$metadata":   "$metadata",
		"embedding_2": "embedding_2",
	} {
		if got := s.columnName(key); got != want {
			t.Errorf("columnName(%q) = %q, want %q", key, got, want)
		}
		back, ok := s.dynamicKey(want)
		if !ok || back != key {
			t.Errorf("dynamicKey(%q) = %q, %v; want %q", want, back, ok, key)
		}
	}
	if _, ok := s.dynamicKey("name"); ok {
		t.Error("a static field is not a dynamic key")
	}
}

func TestTemplateValue(t *testing.T) {
	for _, tc := range []struct {
		v    driver.Value
		ok   bool
		test func(t *testing.T, v driver.Value)
	}{
		{driver.Int(3), true, nil},
		{driver.Float(1.5), true, nil},
		{driver.Bool(true), true, nil},
		{driver.String("x"), true, nil},
		{driver.Decimal("12.50"), true, nil},
		{driver.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), true, nil},
		{driver.List(driver.String("a"), driver.String("b")), true, nil},
		{driver.Null(), false, nil},
		{driver.ObjectValue(), false, nil},
		{driver.Float(math.NaN()), false, nil},
		{driver.Decimal("1e999"), false, nil},
		{driver.Bytes([]byte{1}), false, nil},
	} {
		_, err := templateValue(tc.v)
		if tc.ok && err != nil {
			t.Errorf("templateValue(%s %s) = %v", tc.v.Type(), tc.v, err)
		}
		if !tc.ok && !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("templateValue(%s %s) = %v, want ErrUnsupported", tc.v.Type(), tc.v, err)
		}
	}
	tv, _ := templateValue(driver.Decimal("12.50"))
	if tv.GetFloatVal() != 12.5 {
		t.Errorf("decimal template = %v", tv)
	}
	tv, _ = templateValue(driver.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if tv.GetStringVal() != "2026-01-01T00:00:00Z" {
		t.Errorf("time template = %v", tv)
	}
}

func TestColumnFor(t *testing.T) {
	s := peopleSchema()
	f := func(name string) *fieldInfo { fi, _ := s.field(name); return fi }
	col, err := columnFor(f("age"), driver.Null(), "insert")
	if err != nil {
		t.Fatal(err)
	}
	if isNull, _ := col.IsNull(0); !isNull || col.Len() != 1 {
		t.Errorf("a NULL age is not a one-row null column")
	}
	col, err = columnFor(f("name"), driver.String("Ada"), "insert")
	if err != nil || col.Type() != entity.FieldTypeVarChar {
		t.Fatalf("name column: %v %v", col, err)
	}
	col, err = columnFor(f("embedding"), driver.List(driver.Int(1), driver.Float(0.5)), "insert")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := col.Get(0); len(v.(entity.FloatVector)) != 2 {
		t.Errorf("embedding = %v", v)
	}
	col, err = columnFor(f("meta"), driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}), "insert")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := col.Get(0); string(v.([]byte)) != `{"a":1}` {
		t.Errorf("meta = %s", v)
	}
	for _, tc := range []struct {
		field string
		v     driver.Value
	}{
		{"name", driver.Null()},                   // not nullable
		{"age", driver.String("x")},               // not a number
		{"age", driver.Float(1.5)},                // not whole
		{"embedding", driver.List(driver.Int(1))}, // wrong dimension
		{"embedding", driver.String("1,2")},       // not a vector
		{"embedding", driver.Vector([]float32{float32(math.NaN()), 1})},
	} {
		if _, err := columnFor(f(tc.field), tc.v, "insert"); !errors.Is(err, driver.ErrSyntax) && !errors.Is(err, driver.ErrConstraint) {
			t.Errorf("columnFor(%s, %s) = %v, want ErrSyntax or ErrConstraint", tc.field, tc.v, err)
		}
	}
}
