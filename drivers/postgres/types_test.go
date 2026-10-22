package postgres

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/xavidop/tobira/driver"
)

func encode(t *testing.T, m *pgtype.Map, oid uint32, format int16, v any) []byte {
	t.Helper()
	b, err := m.Encode(oid, format, v, nil)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return b
}

func TestValueFromPG(t *testing.T) {
	m := pgtype.NewMap()
	const vectorOID = 70001 // pgvector has no fixed OID
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	zoned := time.Date(2024, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	uid := [16]byte{0x12, 0x3e, 0x45, 0x67, 0xe8, 0x9b, 0x12, 0xd3, 0xa4, 0x56, 0x42, 0x66, 0x14, 0x17, 0x40, 0x00}
	const uidText = "123e4567-e89b-12d3-a456-426614174000"

	tests := []struct {
		name     string
		oid      uint32
		typeName string
		format   int16
		raw      []byte
		want     driver.Value
	}{
		{"int2 text", pgtype.Int2OID, "", 0, []byte("7"), driver.Int(7)},
		{"int4 binary", pgtype.Int4OID, "", 1, encode(t, m, pgtype.Int4OID, 1, int32(-42)), driver.Int(-42)},
		{"int8 binary", pgtype.Int8OID, "", 1, encode(t, m, pgtype.Int8OID, 1, int64(1<<40)), driver.Int(1 << 40)},
		{"numeric text", pgtype.NumericOID, "", 0, []byte("12.50"), driver.Decimal("12.50")},
		{"numeric binary keeps scale", pgtype.NumericOID, "", 1, mustNumericBinary(t, m, "12.50"), driver.Decimal("12.50")},
		{"numeric binary big", pgtype.NumericOID, "", 1, mustNumericBinary(t, m, "-0.0010"), driver.Decimal("-0.0010")},
		{"numeric binary zero fraction", pgtype.NumericOID, "", 1, mustNumericBinary(t, m, "1200.00"), driver.Decimal("1200.00")},
		{"numeric binary integer", pgtype.NumericOID, "", 1, mustNumericBinary(t, m, "1200"), driver.Decimal("1200")},
		{"numeric nan", pgtype.NumericOID, "", 0, []byte("NaN"), driver.Decimal("NaN")},
		{"float4", pgtype.Float4OID, "", 1, encode(t, m, pgtype.Float4OID, 1, float32(1.5)), driver.Float(1.5)},
		{"float8", pgtype.Float8OID, "", 0, []byte("2.25"), driver.Float(2.25)},
		{"bool", pgtype.BoolOID, "", 0, []byte("t"), driver.Bool(true)},
		{"text", pgtype.TextOID, "", 0, []byte("héllo"), driver.String("héllo")},
		{"varchar", pgtype.VarcharOID, "", 1, []byte("abc"), driver.String("abc")},
		{"uuid binary", pgtype.UUIDOID, "", 1, uid[:], driver.String(uidText)},
		{"uuid text", pgtype.UUIDOID, "", 0, []byte(uidText), driver.String(uidText)},
		{"bytea binary", pgtype.ByteaOID, "", 1, []byte{0, 1, 2}, driver.Bytes([]byte{0, 1, 2})},
		{"bytea text", pgtype.ByteaOID, "", 0, []byte(`\x000102`), driver.Bytes([]byte{0, 1, 2})},
		{"timestamptz binary", pgtype.TimestamptzOID, "", 1, encode(t, m, pgtype.TimestamptzOID, 1, zoned), driver.Time(zoned.UTC())},
		{"timestamp text is UTC", pgtype.TimestampOID, "", 0, []byte("2024-01-02 03:04:05"), driver.Time(ts)},
		{"timestamp binary is UTC", pgtype.TimestampOID, "", 1, encode(t, m, pgtype.TimestampOID, 1, ts), driver.Time(ts)},
		{"date", pgtype.DateOID, "", 0, []byte("2024-01-02"), driver.Time(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))},
		{"timestamptz infinity", pgtype.TimestamptzOID, "", 0, []byte("infinity"), driver.Unknown("timestamptz", []byte("infinity"), "infinity")},
		{"json", pgtype.JSONOID, "", 0, []byte(`{"a": 1}`), driver.JSON([]byte(`{"a": 1}`))},
		{"jsonb binary", pgtype.JSONBOID, "", 1, append([]byte{1}, `{"a":1}`...), driver.JSON([]byte(`{"a":1}`))},
		{"int4[] text", pgtype.Int4ArrayOID, "", 0, []byte("{1,2,NULL}"), driver.List(driver.Int(1), driver.Int(2), driver.Null())},
		{"int4[] binary", pgtype.Int4ArrayOID, "", 1, encode(t, m, pgtype.Int4ArrayOID, 1, []int32{3, 4}), driver.List(driver.Int(3), driver.Int(4))},
		{"text[] text", pgtype.TextArrayOID, "", 0, []byte(`{a,"b c"}`), driver.List(driver.String("a"), driver.String("b c"))},
		{"numeric[] text", pgtype.NumericArrayOID, "", 0, []byte("{1.10,2}"), driver.List(driver.Decimal("1.10"), driver.Decimal("2"))},
		{"int4[][] text", pgtype.Int4ArrayOID, "", 0, []byte("{{1,2},{3,4}}"),
			driver.List(driver.List(driver.Int(1), driver.Int(2)), driver.List(driver.Int(3), driver.Int(4)))},
		{"empty array", pgtype.Int4ArrayOID, "", 0, []byte("{}"), driver.List()},
		{"null", pgtype.Int4OID, "", 0, nil, driver.Null()},
		{"interval", pgtype.IntervalOID, "", 0, []byte("1 day 02:00:00"), driver.Unknown("interval", []byte("1 day 02:00:00"), "1 day 02:00:00")},
		{"unknown type uses given name", 70002, "citext2", 0, []byte("x"), driver.Unknown("citext2", []byte("x"), "x")},
		{"vector text", vectorOID, "vector", 0, []byte("[1,2.5,-3]"), driver.Vector([]float32{1, 2.5, -3})},
		{"vector empty", vectorOID, "vector", 0, []byte("[]"), driver.Vector([]float32{})},
		{"vector binary", vectorOID, "vector", 1, []byte{0, 2, 0, 0, 0x3f, 0x80, 0, 0, 0x40, 0, 0, 0}, driver.Vector([]float32{1, 2})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := valueFromPG(m, tt.oid, tt.format, tt.typeName, tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v (%s), want %#v (%s)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestValueFromPGErrors(t *testing.T) {
	m := pgtype.NewMap()
	for name, tc := range map[string]struct {
		oid      uint32
		typeName string
		raw      string
	}{
		"bad int":    {pgtype.Int4OID, "", "abc"},
		"bad vector": {70001, "vector", "[1,x]"},
		"bad array":  {pgtype.Int4ArrayOID, "", "{1,"},
	} {
		if _, err := valueFromPG(m, tc.oid, 0, tc.typeName, []byte(tc.raw)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func mustNumericBinary(t *testing.T, m *pgtype.Map, s string) []byte {
	t.Helper()
	// Encode the text form through the numeric codec to get the wire binary
	// form, which carries the display scale.
	var n pgtype.Numeric
	if err := m.Scan(pgtype.NumericOID, 0, []byte(s), &n); err != nil {
		t.Fatal(err)
	}
	return encode(t, m, pgtype.NumericOID, 1, n)
}

func TestParamToPG(t *testing.T) {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		in   driver.Value
		want any
	}{
		{"null", driver.Null(), nil},
		{"bool", driver.Bool(true), true},
		{"int", driver.Int(5), int64(5)},
		{"float", driver.Float(1.5), 1.5},
		{"decimal", driver.Decimal("12.50"), "12.50"},
		{"string", driver.String("x"), "x"},
		{"bytes", driver.Bytes([]byte{1, 2}), []byte{1, 2}},
		{"time", driver.Time(ts), ts},
		{"json", driver.JSON([]byte(`{"a":1}`)), []byte(`{"a":1}`)},
		{"vector", driver.Vector([]float32{1, 2.5, -3}), "[1,2.5,-3]"},
		{"empty vector", driver.Vector(nil), "[]"},
		{"list", driver.List(driver.Int(1), driver.String("a"), driver.Null(), driver.List(driver.Bool(false))),
			[]any{int64(1), "a", nil, []any{false}}},
		{"object", driver.ObjectValue(driver.Member{Key: "k", Value: driver.Decimal("1.0")}, driver.Member{Key: "n", Value: driver.Null()}),
			map[string]any{"k": "1.0", "n": nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paramToPG(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
	// Every produced value must be encodable by pgx for a suitable type.
	m := pgtype.NewMap()
	oids := map[string]uint32{
		"null": pgtype.TextOID, "bool": pgtype.BoolOID, "int": pgtype.Int8OID, "float": pgtype.Float8OID,
		"decimal": pgtype.NumericOID, "string": pgtype.TextOID, "bytes": pgtype.ByteaOID,
		"time": pgtype.TimestamptzOID, "json": pgtype.JSONBOID, "vector": pgtype.TextOID,
		"empty vector": pgtype.TextOID, "list": pgtype.JSONBOID, "object": pgtype.JSONBOID,
	}
	for _, tt := range tests {
		got, _ := paramToPG(tt.in)
		formats := []int16{pgtype.TextFormatCode, pgtype.BinaryFormatCode}
		if _, isStr := got.(string); isStr { // pgx sends strings as text for any OID
			formats = formats[:1]
		}
		for _, f := range formats {
			if _, err := m.Encode(oids[tt.name], f, got, nil); err != nil {
				t.Errorf("%s format %d: %v", tt.name, f, err)
			}
		}
	}
	arr := mustParam(t, driver.List(driver.Int(1), driver.Int(2)))
	if _, err := m.Encode(pgtype.Int8ArrayOID, pgtype.BinaryFormatCode, arr, nil); err != nil {
		t.Errorf("int8[]: %v", err)
	}
	if got, _ := paramToPG(driver.Vector([]float32{1})); got == nil {
		t.Fatal("nil")
	}
	if _, err := json.Marshal(mustParam(t, driver.ObjectValue())); err != nil {
		t.Error(err)
	}
	if _, err := m.Encode(pgtype.TextOID, 0, mustParam(t, driver.Decimal("1")), nil); err != nil {
		t.Error(err)
	}
}

func mustParam(t *testing.T, v driver.Value) any {
	t.Helper()
	got, err := paramToPG(v)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestParamToPGUnknown(t *testing.T) {
	got, err := paramToPG(driver.Unknown("interval", []byte("1 day"), "1 day"))
	if err != nil || got != "1 day" {
		t.Errorf("got %#v, %v", got, err)
	}
}
