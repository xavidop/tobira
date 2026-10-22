package qdrant

import (
	"errors"
	"math"
	"testing"
	"time"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestFromValue(t *testing.T) {
	st := pb.NewValueFromFields(map[string]*pb.Value{
		"b": pb.NewValueInt(2),
		"a": pb.NewValueString("x"),
	})
	tests := []struct {
		in   *pb.Value
		want driver.Value
	}{
		{nil, driver.Null()},
		{pb.NewValueNull(), driver.Null()},
		{pb.NewValueBool(true), driver.Bool(true)},
		{pb.NewValueInt(-7), driver.Int(-7)},
		{pb.NewValueDouble(1.5), driver.Float(1.5)},
		{pb.NewValueString("héllo"), driver.String("héllo")},
		{pb.NewValueFromList(pb.NewValueInt(1), pb.NewValueString("two")), driver.List(driver.Int(1), driver.String("two"))},
		{st, driver.ObjectValue(driver.Member{Key: "a", Value: driver.String("x")}, driver.Member{Key: "b", Value: driver.Int(2)})},
		{&pb.Value{}, driver.Null()}, // no kind set
	}
	for _, tt := range tests {
		got := fromValue(tt.in)
		if !drivertest.EqualValue(tt.want, got) || got.Type() != tt.want.Type() {
			t.Errorf("fromValue(%v) = %s (%s), want %s (%s)", tt.in, got, got.Type(), tt.want, tt.want.Type())
		}
	}
	// Object members come out in key order, so rows are deterministic.
	obj, _ := fromValue(st).AsObject()
	if obj[0].Key != "a" || obj[1].Key != "b" {
		t.Errorf("object order = %v", obj)
	}
}

func TestTypedValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		in   *pb.Value
		typ  fieldType
		want driver.Value
		wt   driver.Type
	}{
		{"decimal from string", pb.NewValueString("12.50"), typeDecimal, driver.Decimal("12.50"), driver.TypeDecimal},
		{"decimal from int", pb.NewValueInt(12), typeDecimal, driver.Decimal("12"), driver.TypeDecimal},
		{"decimal from double", pb.NewValueDouble(0.25), typeDecimal, driver.Decimal("0.25"), driver.TypeDecimal},
		{"decimal junk stays string", pb.NewValueString("n/a"), typeDecimal, driver.String("n/a"), driver.TypeString},
		{"time rfc3339", pb.NewValueString("2026-01-02T03:04:05Z"), typeTime, driver.Time(ts), driver.TypeTime},
		{"time offset", pb.NewValueString("2026-01-02T04:04:05+01:00"), typeTime, driver.Time(ts), driver.TypeTime},
		{"time space", pb.NewValueString("2026-01-02 03:04:05"), typeTime, driver.Time(ts), driver.TypeTime},
		{"time junk stays string", pb.NewValueString("yesterday"), typeTime, driver.String("yesterday"), driver.TypeString},
		{"json list", pb.NewValueFromList(pb.NewValueString("a"), pb.NewValueString("b")), typeJSON, driver.JSON([]byte(`["a","b"]`)), driver.TypeJSON},
		{"json empty list", pb.NewValueFromList(), typeJSON, driver.JSON([]byte(`[]`)), driver.TypeJSON},
		{"json string", pb.NewValueString("x"), typeJSON, driver.JSON([]byte(`"x"`)), driver.TypeJSON},
		{"null stays null", pb.NewValueNull(), typeDecimal, driver.Null(), driver.TypeNull},
		{"natural", pb.NewValueInt(3), typeNatural, driver.Int(3), driver.TypeInt},
		{"int above 2^53 stays exact", pb.NewValueInt(9007199254740993), typeInt, driver.Int(9007199254740993), driver.TypeInt},
		{"int min", pb.NewValueInt(math.MinInt64), typeInt, driver.Int(math.MinInt64), driver.TypeInt},
		{"int from integral double", pb.NewValueDouble(3), typeInt, driver.Int(3), driver.TypeInt},
		{"int from huge double stays float", pb.NewValueDouble(1e20), typeInt, driver.Float(1e20), driver.TypeFloat},
		{"int from fraction stays float", pb.NewValueDouble(2.5), typeInt, driver.Float(2.5), driver.TypeFloat},
		{"string of int", pb.NewValueInt(3), typeString, driver.String("3"), driver.TypeString},
	}
	for _, tt := range tests {
		got := typedValue(tt.in, tt.typ)
		if got.Type() != tt.wt || !drivertest.EqualValue(tt.want, got) {
			t.Errorf("%s: got %s (%s), want %s (%s)", tt.name, got, got.Type(), tt.want, tt.wt)
		}
	}
}

func TestToValue(t *testing.T) {
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	tests := []struct {
		in   driver.Value
		want *pb.Value
	}{
		{driver.Null(), pb.NewValueNull()},
		{driver.Bool(false), pb.NewValueBool(false)},
		{driver.Int(9), pb.NewValueInt(9)},
		{driver.Float(2.5), pb.NewValueDouble(2.5)},
		{driver.Decimal("1.50"), pb.NewValueString("1.50")},
		{driver.String("s"), pb.NewValueString("s")},
		{driver.Bytes([]byte{0xff, 0}), pb.NewValueString("/wA=")},
		{driver.Time(ts), pb.NewValueString("2026-06-01T11:00:00Z")},
		{driver.JSON([]byte(`{"a":[1,2.5,"x",null,true]}`)), pb.NewValueFromFields(map[string]*pb.Value{
			"a": pb.NewValueFromList(pb.NewValueInt(1), pb.NewValueDouble(2.5), pb.NewValueString("x"), pb.NewValueNull(), pb.NewValueBool(true)),
		})},
		{driver.Vector([]float32{1, 0.5}), pb.NewValueFromList(pb.NewValueDouble(1), pb.NewValueDouble(0.5))},
		{driver.List(driver.Int(1)), pb.NewValueFromList(pb.NewValueInt(1))},
		{driver.ObjectValue(driver.Member{Key: "k", Value: driver.String("v")}), pb.NewValueFromFields(map[string]*pb.Value{"k": pb.NewValueString("v")})},
	}
	for _, tt := range tests {
		got, err := toValue(tt.in)
		if err != nil {
			t.Errorf("toValue(%s): %v", tt.in, err)
			continue
		}
		if got.String() != tt.want.String() {
			t.Errorf("toValue(%s) = %v, want %v", tt.in, got, tt.want)
		}
	}
	for _, bad := range []driver.Value{driver.Float(math.NaN()), driver.Float(math.Inf(1)), driver.JSON([]byte(`{bad`))} {
		if _, err := toValue(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("toValue(%s): err = %v, want ErrSyntax", bad, err)
		}
	}
}

// TestTypedIntExact compares the int64 itself: EqualValue alone would not
// catch a float round trip that lands on a neighbour.
func TestTypedIntExact(t *testing.T) {
	for _, n := range []int64{1<<53 + 1, math.MaxInt64, math.MinInt64 + 1} {
		got, ok := typedValue(pb.NewValueInt(n), typeInt).AsInt()
		if !ok || got != n {
			t.Errorf("typedValue(%d, int) = %d", n, got)
		}
	}
}

// TestEncodeLike: a Decimal written over a stored JSON number stays a
// number; over a string, or with nothing stored, it is text.
func TestEncodeLike(t *testing.T) {
	for _, tt := range []struct {
		in   driver.Value
		cur  *pb.Value
		want *pb.Value
	}{
		{driver.Decimal("12.50"), pb.NewValueDouble(1), pb.NewValueDouble(12.5)},
		{driver.Decimal("7"), pb.NewValueInt(1), pb.NewValueInt(7)},
		{driver.Decimal("9007199254740993"), pb.NewValueInt(1), pb.NewValueInt(9007199254740993)},
		{driver.Decimal("7"), pb.NewValueDouble(1), pb.NewValueInt(7)},
		{driver.Decimal("12.50"), pb.NewValueString("1"), pb.NewValueString("12.50")},
		{driver.Decimal("12.50"), nil, pb.NewValueString("12.50")},
		{driver.Decimal("1e400"), pb.NewValueDouble(1), pb.NewValueString("1e400")},
		{driver.String("x"), pb.NewValueDouble(1), pb.NewValueString("x")},
	} {
		got, err := encodeLike(tt.in, tt.cur)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != tt.want.String() {
			t.Errorf("encodeLike(%s, %v) = %v, want %v", tt.in, tt.cur, got, tt.want)
		}
	}
}

func TestPointIDValues(t *testing.T) {
	if got := idValue(pb.NewIDNum(7)); !drivertest.EqualValue(driver.Int(7), got) || got.Type() != driver.TypeInt {
		t.Errorf("num id = %s", got)
	}
	if got := idValue(pb.NewIDNum(math.MaxUint64)); got.Type() != driver.TypeDecimal {
		t.Errorf("huge id = %s (%s), want a Decimal", got, got.Type())
	}
	u := "5c56c793-69f3-4fbf-87e6-c4bf54c28c26"
	if got := idValue(pb.NewIDUUID(u)); got.String() != u || got.Type() != driver.TypeString {
		t.Errorf("uuid id = %s", got)
	}
	for _, tt := range []struct {
		in   driver.Value
		want string
	}{
		{driver.Int(7), "num:7"},
		{driver.Decimal("18446744073709551615"), "num:18446744073709551615"},
		{driver.String(u), "uuid:" + u},
		{driver.String("12"), "num:12"},
	} {
		id, err := pointID(tt.in)
		if err != nil {
			t.Errorf("pointID(%s): %v", tt.in, err)
			continue
		}
		if got := idString(id); got != tt.want {
			t.Errorf("pointID(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
	for _, bad := range []driver.Value{driver.Int(-1), driver.String("not-a-uuid"), driver.Float(1.5), driver.Null()} {
		if _, err := pointID(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("pointID(%s): err = %v, want ErrSyntax", bad, err)
		}
	}
}

func TestVectorValues(t *testing.T) {
	dense := &pb.VectorOutput{Vector: &pb.VectorOutput_Dense{Dense: &pb.DenseVector{Data: []float32{1, 2}}}}
	if got := vectorValue(dense); got.Type() != driver.TypeVector {
		t.Errorf("dense = %s (%s)", got, got.Type())
	}
	sparse := &pb.VectorOutput{Vector: &pb.VectorOutput_Sparse{Sparse: &pb.SparseVector{Indices: []uint32{3}, Values: []float32{0.5}}}}
	if got := vectorValue(sparse); got.Type() != driver.TypeObject {
		t.Errorf("sparse = %s (%s), want an Object", got, got.Type())
	}
	multi := &pb.VectorOutput{Vector: &pb.VectorOutput_MultiDense{MultiDense: &pb.MultiDenseVector{Vectors: []*pb.DenseVector{{Data: []float32{1}}, {Data: []float32{2}}}}}}
	if got := vectorValue(multi); got.Type() != driver.TypeList {
		t.Errorf("multi = %s (%s), want a List", got, got.Type())
	}
	legacy := &pb.VectorOutput{Data: []float32{4, 5}} //nolint:staticcheck // servers before 1.12 fill only Data
	if got := vectorValue(legacy); got.Type() != driver.TypeVector {
		t.Errorf("legacy = %s (%s)", got, got.Type())
	}
	if got := vectorValue(nil); !got.IsNull() {
		t.Errorf("nil = %s", got)
	}
}
