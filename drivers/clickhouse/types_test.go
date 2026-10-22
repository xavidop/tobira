package clickhouse

import (
	"math"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/xavidop/tobira/driver"
)

func TestColumnType(t *testing.T) {
	for native, want := range map[string]driver.Type{
		"Int8": driver.TypeInt, "Int64": driver.TypeInt, "UInt8": driver.TypeInt, "UInt32": driver.TypeInt,
		"UInt64": driver.TypeDecimal, "Int128": driver.TypeDecimal, "UInt256": driver.TypeDecimal,
		"Float32": driver.TypeFloat, "Float64": driver.TypeFloat, "BFloat16": driver.TypeFloat,
		"Decimal(10, 2)": driver.TypeDecimal, "Decimal64(4)": driver.TypeDecimal,
		"Bool": driver.TypeBool, "String": driver.TypeString, "FixedString(3)": driver.TypeString,
		"UUID": driver.TypeString, "IPv4": driver.TypeString, "IPv6": driver.TypeString,
		"Enum8('a' = 1, 'b' = 2)": driver.TypeString,
		"Date":                    driver.TypeTime, "Date32": driver.TypeTime, "DateTime": driver.TypeTime,
		"DateTime('UTC')": driver.TypeTime, "DateTime64(3, 'UTC')": driver.TypeTime,
		"Nullable(Int32)": driver.TypeInt, "LowCardinality(String)": driver.TypeString,
		"LowCardinality(Nullable(String))": driver.TypeString,
		"Array(String)":                    driver.TypeJSON, "Map(String, UInt64)": driver.TypeJSON,
		"Tuple(a Int8, b String)": driver.TypeJSON, "JSON": driver.TypeJSON, "Nested(a Int8)": driver.TypeJSON,
		"Variant(String, UInt64)": driver.TypeJSON, "Dynamic": driver.TypeJSON, "Point": driver.TypeJSON,
		"Nothing": driver.TypeNull, "Nullable(Nothing)": driver.TypeNull,
		"IntervalSecond": driver.TypeInt, "AggregateFunction(sum, UInt64)": driver.TypeUnknown,
		"SomethingNew": driver.TypeUnknown,
	} {
		if got := columnType(native); got != want {
			t.Errorf("columnType(%q) = %v, want %v", native, got, want)
		}
	}
}

func TestNullable(t *testing.T) {
	for native, want := range map[string]bool{
		"Int32": false, "Nullable(Int32)": true, "LowCardinality(Nullable(String))": true, "Array(Nullable(Int8))": false,
	} {
		if got := nullable(native); got != want {
			t.Errorf("nullable(%q) = %v, want %v", native, got, want)
		}
	}
}

func TestConvertValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	i8, s := int8(-3), "x"
	var nilInt *int32
	big1, _ := new(big.Int).SetString("170141183460469231731687303715884105727", 10)
	u := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	tests := []struct {
		name   string
		native string
		in     any
		want   driver.Value
	}{
		{"nil", "Nullable(Int32)", nil, driver.Null()},
		{"nil pointer", "Nullable(Int32)", nilInt, driver.Null()},
		{"pointer", "Nullable(Int8)", &i8, driver.Int(-3)},
		{"bool", "Bool", true, driver.Bool(true)},
		{"int8", "Int8", int8(-1), driver.Int(-1)},
		{"uint32", "UInt32", uint32(math.MaxUint32), driver.Int(math.MaxUint32)},
		{"uint64 small", "UInt64", uint64(42), driver.Decimal("42")},
		{"uint64 overflow", "UInt64", uint64(math.MaxUint64), driver.Decimal("18446744073709551615")},
		{"float32", "Float32", float32(1.5), driver.Float(1.5)},
		{"float64", "Float64", 2.25, driver.Float(2.25)},
		{"string", "String", "héllo", driver.String("héllo")},
		{"string pointer", "Nullable(String)", &s, driver.String("x")},
		{"binary string", "String", "\xff\xfe", driver.Bytes([]byte{0xff, 0xfe})},
		{"time", "DateTime('UTC')", ts, driver.Time(ts)},
		{"decimal scale", "Decimal(10, 2)", decimal.RequireFromString("12.5"), driver.Decimal("12.50")},
		{"decimal no scale", "Decimal", decimal.RequireFromString("7.25"), driver.Decimal("7.25")},
		{"big int", "Int128", big1, driver.Decimal("170141183460469231731687303715884105727")},
		{"uuid", "UUID", u, driver.String("6ba7b810-9dad-11d1-80b4-00c04fd430c8")},
		{"ip", "IPv4", net.IPv4(10, 0, 0, 1), driver.String("10.0.0.1")},
		{"time of day", "Time", 26*time.Hour + 3*time.Minute + 4*time.Second, driver.String("26:03:04")},
		{"time64", "Time64(3)", -(time.Hour + 500*time.Millisecond), driver.String("-01:00:00.5")},
		{"interval", "IntervalSecond", int64(5), driver.Int(5)},
		{"array", "Array(String)", []string{"a", "b"}, driver.JSON([]byte(`["a","b"]`))},
		{"empty array", "Array(String)", []string{}, driver.JSON([]byte(`[]`))},
		{"map", "Map(String, UInt64)", map[string]uint64{"k": 1}, driver.JSON([]byte(`{"k":1}`))},
		{"nested decimals", "Array(Decimal(5, 1))", []decimal.Decimal{decimal.RequireFromString("1.5")}, driver.JSON([]byte(`["1.5"]`))},
		{"array with NaN stays JSON", "Array(Float64)", []float64{math.NaN(), 1}, driver.JSON([]byte(`"[NaN 1]"`))},
		{"map with float keys", "Map(Float64, UInt8)", map[float64]uint8{1.5: 1}, driver.JSON([]byte(`{"1.5":1}`))},
		{"json text", "JSON", `{"a":1}`, driver.JSON([]byte(`{"a":1}`))},
		{"unknown type", "AggregateFunction(sum, UInt64)", []byte{1, 2}, driver.Unknown("AggregateFunction(sum, UInt64)", []byte{1, 2}, "AQI=")},
		{"unmarshalable", "Dynamic", complex(1, 2), driver.JSON([]byte(`"(1+2i)"`))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertValue(tt.native, tt.in)
			if got.Type() != tt.want.Type() || got.String() != tt.want.String() {
				t.Errorf("convertValue(%q, %#v) = %s %q, want %s %q", tt.native, tt.in, got.Type(), got, tt.want.Type(), tt.want)
			}
		})
	}
}
