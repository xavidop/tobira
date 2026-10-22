package influxdb

import (
	"math"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// build makes a one-or-two-element array of dt with fill and releases it at
// the end of the test.
func build(t *testing.T, dt arrow.DataType, fill func(b array.Builder)) arrow.Array {
	t.Helper()
	b := array.NewBuilder(memory.NewGoAllocator(), dt)
	defer b.Release()
	fill(b)
	a := b.NewArray()
	t.Cleanup(a.Release)
	return a
}

func TestValueAt(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	cases := []struct {
		name string
		arr  arrow.Array
		want driver.Value
		typ  driver.Type
	}{
		{"int64", build(t, arrow.PrimitiveTypes.Int64, func(b array.Builder) { b.(*array.Int64Builder).Append(-7) }), driver.Int(-7), driver.TypeInt},
		{"int32", build(t, arrow.PrimitiveTypes.Int32, func(b array.Builder) { b.(*array.Int32Builder).Append(9) }), driver.Int(9), driver.TypeInt},
		{"uint64 small", build(t, arrow.PrimitiveTypes.Uint64, func(b array.Builder) { b.(*array.Uint64Builder).Append(42) }), driver.Decimal("42"), driver.TypeDecimal},
		{"uint64 big", build(t, arrow.PrimitiveTypes.Uint64, func(b array.Builder) { b.(*array.Uint64Builder).Append(math.MaxUint64) }), driver.Decimal("18446744073709551615"), driver.TypeDecimal},
		{"float64", build(t, arrow.PrimitiveTypes.Float64, func(b array.Builder) { b.(*array.Float64Builder).Append(1.5) }), driver.Float(1.5), driver.TypeFloat},
		{"bool", build(t, arrow.FixedWidthTypes.Boolean, func(b array.Builder) { b.(*array.BooleanBuilder).Append(true) }), driver.Bool(true), driver.TypeBool},
		{"utf8", build(t, arrow.BinaryTypes.String, func(b array.Builder) { b.(*array.StringBuilder).Append("Ōtani") }), driver.String("Ōtani"), driver.TypeString},
		{"large utf8", build(t, arrow.BinaryTypes.LargeString, func(b array.Builder) { b.(*array.LargeStringBuilder).Append("x") }), driver.String("x"), driver.TypeString},
		{"binary", build(t, arrow.BinaryTypes.Binary, func(b array.Builder) { b.(*array.BinaryBuilder).Append([]byte{1, 2}) }), driver.Bytes([]byte{1, 2}), driver.TypeBytes},
		{"timestamp ns", build(t, &arrow.TimestampType{Unit: arrow.Nanosecond}, func(b array.Builder) {
			b.(*array.TimestampBuilder).Append(arrow.Timestamp(ts.UnixNano()))
		}), driver.Time(ts), driver.TypeTime},
		{"timestamp tz", build(t, &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: "Europe/Madrid"}, func(b array.Builder) {
			b.(*array.TimestampBuilder).Append(arrow.Timestamp(ts.UnixMilli()))
		}), driver.Time(ts.Truncate(time.Millisecond)), driver.TypeTime},
		{"date32", build(t, arrow.FixedWidthTypes.Date32, func(b array.Builder) { b.(*array.Date32Builder).Append(arrow.Date32FromTime(ts)) }),
			driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)), driver.TypeTime},
		{"decimal", build(t, &arrow.Decimal128Type{Precision: 10, Scale: 2}, func(b array.Builder) {
			b.(*array.Decimal128Builder).Append(decimal128.FromI64(1250))
		}), driver.Decimal("12.50"), driver.TypeDecimal},
		{"null in int", build(t, arrow.PrimitiveTypes.Int64, func(b array.Builder) { b.AppendNull() }), driver.Null(), driver.TypeInt},
		{"null type", build(t, arrow.Null, func(b array.Builder) { b.AppendNull() }), driver.Null(), driver.TypeNull},
		{"duration", build(t, arrow.FixedWidthTypes.Duration_s, func(b array.Builder) { b.(*array.DurationBuilder).Append(90) }), driver.String("1m30s"), driver.TypeString},
		{"list", build(t, arrow.ListOf(arrow.PrimitiveTypes.Int64), func(b array.Builder) {
			lb := b.(*array.ListBuilder)
			lb.Append(true)
			lb.ValueBuilder().(*array.Int64Builder).AppendValues([]int64{1, 2}, nil)
		}), driver.List(driver.Int(1), driver.Int(2)), driver.TypeList},
		{"struct", build(t, arrow.StructOf(arrow.Field{Name: "a", Type: arrow.PrimitiveTypes.Int64}, arrow.Field{Name: "b", Type: arrow.BinaryTypes.String}), func(b array.Builder) {
			sb := b.(*array.StructBuilder)
			sb.Append(true)
			sb.FieldBuilder(0).(*array.Int64Builder).Append(1)
			sb.FieldBuilder(1).(*array.StringBuilder).Append("x")
		}), driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}, driver.Member{Key: "b", Value: driver.String("x")}), driver.TypeObject},
	}
	// A tag column: dictionary-encoded strings.
	dict := build(t, &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.String}, func(b array.Builder) {
		db := b.(*array.BinaryDictionaryBuilder)
		_ = db.AppendString("web-1")
		_ = db.AppendString("web-2")
	})
	cases = append(cases, struct {
		name string
		arr  arrow.Array
		want driver.Value
		typ  driver.Type
	}{"dictionary", dict, driver.String("web-2"), driver.TypeString})

	for _, tc := range cases {
		i := tc.arr.Len() - 1
		got := valueAt(tc.arr, i)
		if !drivertest.EqualValue(tc.want, got) || got.Type() != tc.want.Type() {
			t.Errorf("%s: valueAt = %s (%s), want %s (%s)", tc.name, got, got.Type(), tc.want, tc.want.Type())
		}
		if typ := columnType(tc.arr.DataType()); typ != tc.typ {
			t.Errorf("%s: columnType = %s, want %s", tc.name, typ, tc.typ)
		}
	}
}

func TestValueAtUnknownNeverPanics(t *testing.T) {
	arr := build(t, arrow.FixedWidthTypes.MonthInterval, func(b array.Builder) { b.(*array.MonthIntervalBuilder).Append(3) })
	got := valueAt(arr, 0)
	if got.IsNull() {
		t.Fatal("an interval value became NULL")
	}
	if s := got.String(); s == "" {
		t.Error("an interval value has no display text")
	}
}

func TestFieldTypeFromInfo(t *testing.T) {
	for native, want := range map[string]driver.Type{
		"Int64":                       driver.TypeInt,
		"UInt64":                      driver.TypeDecimal,
		"UInt32":                      driver.TypeInt,
		"Float64":                     driver.TypeFloat,
		"Utf8":                        driver.TypeString,
		"Dictionary(Int32, Utf8)":     driver.TypeString,
		"Boolean":                     driver.TypeBool,
		"Timestamp(ns)":               driver.TypeTime,
		"Timestamp(Nanosecond, None)": driver.TypeTime,
		"Decimal128(10, 2)":           driver.TypeDecimal,
		"Binary":                      driver.TypeBytes,
		"Interval(MonthDayNano)":      driver.TypeUnknown,
	} {
		if got := fieldType(native); got != want {
			t.Errorf("fieldType(%q) = %s, want %s", native, got, want)
		}
	}
}

// TestFieldTypeMatchesColumnType checks that Describe (which reads the
// information_schema spelling) and Query (which reads the Arrow type) give
// a column the same Type.
func TestFieldTypeMatchesColumnType(t *testing.T) {
	for native, dt := range map[string]arrow.DataType{
		"Int64":                         arrow.PrimitiveTypes.Int64,
		"UInt32":                        arrow.PrimitiveTypes.Uint32,
		"UInt64":                        arrow.PrimitiveTypes.Uint64,
		"Float64":                       arrow.PrimitiveTypes.Float64,
		"Boolean":                       arrow.FixedWidthTypes.Boolean,
		"Utf8":                          arrow.BinaryTypes.String,
		"Dictionary(Int32, Utf8)":       &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.String},
		"Timestamp(ns)":                 &arrow.TimestampType{Unit: arrow.Nanosecond},
		"Date32":                        arrow.FixedWidthTypes.Date32,
		"Decimal128(10, 2)":             &arrow.Decimal128Type{Precision: 10, Scale: 2},
		"Binary":                        arrow.BinaryTypes.Binary,
		"Duration(ns)":                  arrow.FixedWidthTypes.Duration_ns,
		"Duration(Nanosecond)":          arrow.FixedWidthTypes.Duration_ns,
		"Time64(ns)":                    arrow.FixedWidthTypes.Time64ns,
		"Time32(Second)":                arrow.FixedWidthTypes.Time32s,
		"List(Int64)":                   arrow.ListOf(arrow.PrimitiveTypes.Int64),
		"LargeList(Int64)":              arrow.LargeListOf(arrow.PrimitiveTypes.Int64),
		"FixedSizeList(2 x Int64)":      arrow.FixedSizeListOf(2, arrow.PrimitiveTypes.Int64),
		"Struct(\"a\": Int64)":          arrow.StructOf(arrow.Field{Name: "a", Type: arrow.PrimitiveTypes.Int64}),
		"Map(\"entries\": Struct(...))": arrow.MapOf(arrow.BinaryTypes.String, arrow.PrimitiveTypes.Int64),
		"Null":                          arrow.Null,
		"Interval(MonthDayNano)":        arrow.FixedWidthTypes.MonthDayNanoInterval,
	} {
		if f, c := fieldType(native), columnType(dt); f != c {
			t.Errorf("%s: Describe type %s, Query type %s", native, f, c)
		}
	}
}

// TestDurationOverflow: a second-unit duration past ~292 years must not
// wrap around.
func TestDurationOverflow(t *testing.T) {
	arr := build(t, arrow.FixedWidthTypes.Duration_s, func(b array.Builder) {
		b.(*array.DurationBuilder).Append(arrow.Duration(math.MaxInt64 / 2))
	})
	got := valueAt(arr, 0)
	if want := "4611686018427387903s"; got.String() != want {
		t.Errorf("overflowing duration = %s, want %s", got, want)
	}
}
