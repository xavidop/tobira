package bigquery

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	bq "cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"

	"github.com/xavidop/tobira/driver"
)

func TestFieldTypes(t *testing.T) {
	rec := &bq.FieldSchema{Name: "r", Type: bq.RecordFieldType, Schema: bq.Schema{
		{Name: "a", Type: bq.StringFieldType},
		{Name: "b", Type: bq.IntegerFieldType, Repeated: true},
	}}
	tests := []struct {
		fs     *bq.FieldSchema
		typ    driver.Type
		native string
	}{
		{&bq.FieldSchema{Type: bq.IntegerFieldType}, driver.TypeInt, "INT64"},
		{&bq.FieldSchema{Type: bq.FloatFieldType}, driver.TypeFloat, "FLOAT64"},
		{&bq.FieldSchema{Type: bq.NumericFieldType}, driver.TypeDecimal, "NUMERIC"},
		{&bq.FieldSchema{Type: bq.NumericFieldType, Precision: 10, Scale: 2}, driver.TypeDecimal, "NUMERIC(10, 2)"},
		{&bq.FieldSchema{Type: bq.BigNumericFieldType}, driver.TypeDecimal, "BIGNUMERIC"},
		{&bq.FieldSchema{Type: bq.BooleanFieldType}, driver.TypeBool, "BOOL"},
		{&bq.FieldSchema{Type: bq.StringFieldType}, driver.TypeString, "STRING"},
		{&bq.FieldSchema{Type: bq.StringFieldType, MaxLength: 10}, driver.TypeString, "STRING(10)"},
		{&bq.FieldSchema{Type: bq.BytesFieldType}, driver.TypeBytes, "BYTES"},
		{&bq.FieldSchema{Type: bq.TimestampFieldType}, driver.TypeTime, "TIMESTAMP"},
		{&bq.FieldSchema{Type: bq.DateFieldType}, driver.TypeTime, "DATE"},
		{&bq.FieldSchema{Type: bq.DateTimeFieldType}, driver.TypeTime, "DATETIME"},
		{&bq.FieldSchema{Type: bq.TimeFieldType}, driver.TypeString, "TIME"},
		{&bq.FieldSchema{Type: bq.JSONFieldType}, driver.TypeJSON, "JSON"},
		{&bq.FieldSchema{Type: bq.GeographyFieldType}, driver.TypeString, "GEOGRAPHY"},
		{&bq.FieldSchema{Type: bq.IntervalFieldType}, driver.TypeString, "INTERVAL"},
		{&bq.FieldSchema{Type: bq.RangeFieldType, RangeElementType: &bq.RangeElementType{Type: bq.DateFieldType}}, driver.TypeString, "RANGE<DATE>"},
		{&bq.FieldSchema{Type: bq.StringFieldType, Repeated: true}, driver.TypeList, "ARRAY<STRING>"},
		{rec, driver.TypeObject, "STRUCT<a STRING, b ARRAY<INT64>>"},
		{&bq.FieldSchema{Type: bq.RecordFieldType, Repeated: true, Schema: bq.Schema{{Name: "x", Type: bq.BooleanFieldType}}}, driver.TypeList, "ARRAY<STRUCT<x BOOL>>"},
		{&bq.FieldSchema{Type: "MYSTERY"}, driver.TypeUnknown, "MYSTERY"},
	}
	for _, tt := range tests {
		if got := fieldType(tt.fs); got != tt.typ {
			t.Errorf("fieldType(%s) = %s, want %s", tt.native, got, tt.typ)
		}
		if got := nativeType(tt.fs); got != tt.native {
			t.Errorf("nativeType = %q, want %q", got, tt.native)
		}
	}
}

func TestToValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.FixedZone("x", 3600))
	rat, _ := new(big.Rat).SetString("12.5")
	big38, _ := new(big.Rat).SetString("123456789012345678901234567890.12345678901234567890123456789012345678")
	rec := &bq.FieldSchema{Type: bq.RecordFieldType, Schema: bq.Schema{
		{Name: "a", Type: bq.StringFieldType},
		{Name: "b", Type: bq.IntegerFieldType, Repeated: true},
	}}
	tests := []struct {
		name string
		fs   *bq.FieldSchema
		in   bq.Value
		want driver.Value
	}{
		{"null", &bq.FieldSchema{Type: bq.IntegerFieldType}, nil, driver.Null()},
		{"int", &bq.FieldSchema{Type: bq.IntegerFieldType}, int64(42), driver.Int(42)},
		{"float", &bq.FieldSchema{Type: bq.FloatFieldType}, 1.5, driver.Float(1.5)},
		{"numeric", &bq.FieldSchema{Type: bq.NumericFieldType}, rat, driver.Decimal("12.5")},
		{"numeric int", &bq.FieldSchema{Type: bq.NumericFieldType}, big.NewRat(3, 1), driver.Decimal("3")},
		{"bignumeric", &bq.FieldSchema{Type: bq.BigNumericFieldType}, big38, driver.Decimal("123456789012345678901234567890.12345678901234567890123456789012345678")},
		{"bool", &bq.FieldSchema{Type: bq.BooleanFieldType}, true, driver.Bool(true)},
		{"string", &bq.FieldSchema{Type: bq.StringFieldType}, "héllo", driver.String("héllo")},
		{"bytes", &bq.FieldSchema{Type: bq.BytesFieldType}, []byte{0, 1}, driver.Bytes([]byte{0, 1})},
		{"timestamp", &bq.FieldSchema{Type: bq.TimestampFieldType}, ts, driver.Time(ts.UTC())},
		{"date", &bq.FieldSchema{Type: bq.DateFieldType}, civil.Date{Year: 2026, Month: 3, Day: 4}, driver.Time(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC))},
		{"datetime", &bq.FieldSchema{Type: bq.DateTimeFieldType}, civil.DateTime{Date: civil.Date{Year: 2026, Month: 3, Day: 4}, Time: civil.Time{Hour: 5, Minute: 6, Second: 7}}, driver.Time(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))},
		{"time", &bq.FieldSchema{Type: bq.TimeFieldType}, civil.Time{Hour: 5, Minute: 6, Second: 7, Nanosecond: 123456000}, driver.String("05:06:07.123456")},
		{"json", &bq.FieldSchema{Type: bq.JSONFieldType}, `["a","b"]`, driver.JSON([]byte(`["a","b"]`))},
		{"json not json", &bq.FieldSchema{Type: bq.JSONFieldType}, `nope`, driver.String("nope")},
		{"geography", &bq.FieldSchema{Type: bq.GeographyFieldType}, "POINT(1 2)", driver.String("POINT(1 2)")},
		{"interval", &bq.FieldSchema{Type: bq.IntervalFieldType}, &bq.IntervalValue{Years: 1, Months: 2, Days: 3}, driver.String("1-2 3 0:0:0")},
		{"array", &bq.FieldSchema{Type: bq.StringFieldType, Repeated: true}, []bq.Value{"a", "b"}, driver.List(driver.String("a"), driver.String("b"))},
		{"empty array", &bq.FieldSchema{Type: bq.StringFieldType, Repeated: true}, []bq.Value{}, driver.List()},
		{"struct", rec, []bq.Value{"x", []bq.Value{int64(1), int64(2)}}, driver.ObjectValue(
			driver.Member{Key: "a", Value: driver.String("x")},
			driver.Member{Key: "b", Value: driver.List(driver.Int(1), driver.Int(2))},
		)},
		{"null struct", rec, nil, driver.Null()},
	}
	for _, tt := range tests {
		got := toValue(tt.fs, tt.in)
		if got.Type() != tt.want.Type() || got.String() != tt.want.String() {
			t.Errorf("%s: toValue = %s (%s), want %s (%s)", tt.name, got, got.Type(), tt.want, tt.want.Type())
		}
	}
}

// An ARRAY over the cap is a placeholder, never read element by element.
func TestToValueCapsArrays(t *testing.T) {
	old := maxListElems
	maxListElems = 3
	t.Cleanup(func() { maxListElems = old })
	fs := &bq.FieldSchema{Type: bq.IntegerFieldType, Repeated: true}
	got := toValue(fs, []bq.Value{int64(1), int64(2), int64(3), int64(4)})
	native, _, ok := got.AsUnknown()
	if !ok || native != "ARRAY<INT64>" || !strings.Contains(got.String(), "4 elements") {
		t.Errorf("capped array = %s (%s)", got, got.Type())
	}
	if got := toValue(fs, []bq.Value{int64(1), int64(2), int64(3)}); got.Type() != driver.TypeList {
		t.Errorf("array at the cap = %s", got.Type())
	}
	// The cap counts every element under one value, nested arrays included.
	nested := &bq.FieldSchema{Type: bq.RecordFieldType, Repeated: true, Schema: bq.Schema{{Name: "x", Type: bq.IntegerFieldType, Repeated: true}}}
	got = toValue(nested, []bq.Value{[]bq.Value{[]bq.Value{int64(1), int64(2)}}, []bq.Value{[]bq.Value{int64(3), int64(4)}}})
	if got.Type() != driver.TypeUnknown {
		t.Errorf("nested elements over the cap = %s (%s)", got, got.Type())
	}
}

func TestToParams(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ps, err := toParams("query", []driver.Value{
		driver.Null(), driver.Bool(true), driver.Int(7), driver.Float(1.5), driver.Decimal("12.50"),
		driver.String("s"), driver.Bytes([]byte("b")), driver.Time(ts), driver.JSON([]byte(`{"a":1}`)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 9 {
		t.Fatalf("got %d params", len(ps))
	}
	if ps[2].Value != int64(7) || ps[5].Value != "s" || ps[1].Value != true {
		t.Errorf("params = %+v", ps)
	}
	if r, ok := ps[4].Value.(*big.Rat); !ok || r.FloatString(2) != "12.50" {
		t.Errorf("decimal param = %#v", ps[4].Value)
	}
	for _, bad := range []driver.Value{driver.List(driver.Int(1)), driver.Vector([]float32{1}), driver.Decimal("nope")} {
		if _, err := toParams("query", []driver.Value{bad}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("param %s: want ErrUnsupported, got %v", bad.Type(), err)
		}
	}
}

// One huge STRING, BYTES, JSON or GEOGRAPHY value is cut at the cap (text
// at a UTF-8 boundary), and the cut is reported.
func TestToValueCapsLargeValues(t *testing.T) {
	old := maxValueBytes
	maxValueBytes = 8
	t.Cleanup(func() { maxValueBytes = old })
	for _, tc := range []struct {
		name string
		fs   *bq.FieldSchema
		in   bq.Value
		want driver.Value
	}{
		{"string", &bq.FieldSchema{Type: bq.StringFieldType}, "abcdefghij", driver.String("abcdefgh")},
		{"utf8", &bq.FieldSchema{Type: bq.StringFieldType}, "abcdefgé", driver.String("abcdefg")},
		{"bytes", &bq.FieldSchema{Type: bq.BytesFieldType}, []byte("0123456789"), driver.Bytes([]byte("01234567"))},
		{"json", &bq.FieldSchema{Type: bq.JSONFieldType}, `["aaaaaaaaaa"]`, driver.String(`["aaaaaa`)},
		{"geography", &bq.FieldSchema{Type: bq.GeographyFieldType}, "POINT(1 2) POINT", driver.String("POINT(1 ")},
		{"in an array", &bq.FieldSchema{Type: bq.StringFieldType, Repeated: true}, []bq.Value{"ok", "abcdefghij"}, driver.List(driver.String("ok"), driver.String("abcdefgh"))},
	} {
		got, cut := toValueCut(tc.fs, tc.in)
		if !cut || got.Type() != tc.want.Type() || got.String() != tc.want.String() {
			t.Errorf("%s: toValueCut = %s (%s, cut %v), want %s (%s)", tc.name, got, got.Type(), cut, tc.want, tc.want.Type())
		}
	}
	if _, cut := toValueCut(&bq.FieldSchema{Type: bq.StringFieldType}, "12345678"); cut {
		t.Error("a value at the cap was reported cut")
	}
}
