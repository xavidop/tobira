package vertica

import (
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestColumnType(t *testing.T) {
	for native, want := range map[string]driver.Type{
		// vertica-sql-go's result column names.
		"BOOL": driver.TypeBool, "INT": driver.TypeInt, "FLOAT": driver.TypeFloat, "NUMERIC": driver.TypeDecimal,
		"CHAR": driver.TypeString, "VARCHAR": driver.TypeString, "LONG VARCHAR": driver.TypeString, "UUID": driver.TypeString,
		"DATE": driver.TypeTime, "TIMESTAMP": driver.TypeTime, "TIMESTAMPTZ": driver.TypeTime,
		"TIME": driver.TypeString, "TIMETZ": driver.TypeString, "INTERVAL DAY TO SECOND": driver.TypeString, "INTERVAL YEAR TO MONTH": driver.TypeString,
		"BINARY": driver.TypeBytes, "VARBINARY": driver.TypeBytes, "LONG VARBINARY": driver.TypeBytes,
		// v_catalog.columns data_type spellings.
		"int": driver.TypeInt, "integer": driver.TypeInt, "bigint": driver.TypeInt, "boolean": driver.TypeBool,
		"varchar(80)": driver.TypeString, "char(1)": driver.TypeString, "long varchar(1048576)": driver.TypeString,
		"numeric(10,2)": driver.TypeDecimal, "numeric(37,15)": driver.TypeDecimal, "float": driver.TypeFloat,
		"timestamptz": driver.TypeTime, "timestamp(3)": driver.TypeTime, "date": driver.TypeTime, "time": driver.TypeString,
		"varbinary(80)": driver.TypeBytes, "interval": driver.TypeString, "uuid": driver.TypeString,
		// Complex types arrive as text.
		"Array[int8]": driver.TypeString, "ARRAY[VARCHAR(80)]": driver.TypeString, "Set[int8]": driver.TypeString,
		"Row(a int, b varchar)": driver.TypeString,
		"geometry":              driver.TypeUnknown, "unknown column type oid: 99": driver.TypeUnknown,
	} {
		if got := columnType(native); got != want {
			t.Errorf("columnType(%q) = %s, want %s", native, got, want)
		}
	}
}

func TestConvertValue(t *testing.T) {
	plus2 := time.FixedZone("", 2*3600)
	var cut bool
	tests := []struct {
		native string
		scale  int64
		raw    any
		want   driver.Value
	}{
		{"INT", 0, nil, driver.Null()},
		{"INT", 0, 42, driver.Int(42)},
		{"INT", 0, int64(-7), driver.Int(-7)},
		{"BOOL", 0, true, driver.Bool(true)},
		{"FLOAT", 0, 1.25, driver.Float(1.25)},
		// NUMERIC reaches tobira as float64 (vertica-sql-go's decoding); the
		// scale puts the trailing zeros back.
		{"NUMERIC", 2, 12.5, driver.Decimal("12.50")},
		{"NUMERIC", 0, 3.0, driver.Decimal("3")},
		{"NUMERIC", 3, -0.5, driver.Decimal("-0.500")},
		// Browse reads wide NUMERIC columns as text, exactly.
		{"NUMERIC", 2, "123456789012345678.12", driver.Decimal("123456789012345678.12")},
		{"VARCHAR", 0, "héllo", driver.String("héllo")},
		{"UUID", 0, "6b1c9f5e-0000-4000-8000-000000000000", driver.String("6b1c9f5e-0000-4000-8000-000000000000")},
		{"INTERVAL DAY TO SECOND", 0, "1 02:03:04", driver.String("1 02:03:04")},
		{"Array[int8]", 0, "[1,2,3]", driver.String("[1,2,3]")},
		{"Row(a int)", 0, `{"a":1}`, driver.String(`{"a":1}`)},
		{"VARBINARY", 0, []byte{0, 1, 2}, driver.Bytes([]byte{0, 1, 2})},
		{"DATE", 0, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))},
		// A TIMESTAMP has no zone: its wall clock is kept, read as UTC.
		{"TIMESTAMP", 0, time.Date(2026, 1, 2, 3, 4, 5, 6000, plus2), driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC))},
		{"TIMESTAMPTZ", 0, time.Date(2026, 1, 2, 3, 4, 5, 0, plus2), driver.Time(time.Date(2026, 1, 2, 1, 4, 5, 0, time.UTC))},
		{"TIME", 0, time.Date(0, 1, 1, 13, 14, 15, 500000000, plus2), driver.String("13:14:15.5")},
		{"TIMETZ", 0, time.Date(0, 1, 1, 13, 14, 15, 0, plus2), driver.String("13:14:15+02:00")},
	}
	for _, tt := range tests {
		got := convertValue(tt.native, tt.scale, tt.raw, &cut)
		if got.Type() != tt.want.Type() || got.String() != tt.want.String() {
			t.Errorf("convertValue(%s, %d, %#v) = %s %q, want %s %q", tt.native, tt.scale, tt.raw, got.Type(), got.String(), tt.want.Type(), tt.want.String())
		}
		if ti, ok := tt.want.AsTime(); ok {
			if g, _ := got.AsTime(); !g.Equal(ti) {
				t.Errorf("convertValue(%s) time = %v, want %v", tt.native, g, ti)
			}
		}
	}
	if cut {
		t.Error("no value was long enough to cut, but cut is set")
	}
}

func TestConvertValueCutsLongValues(t *testing.T) {
	var cut bool
	long := strings.Repeat("é", maxValueBytes) // 2 bytes per rune
	v := convertValue("LONG VARCHAR", 0, long, &cut)
	s, _ := v.AsText()
	if !cut || len(s) > maxValueBytes || !strings.HasPrefix(long, s) {
		t.Errorf("long text: cut=%v len=%d, want cut at %d bytes on a rune boundary", cut, len(s), maxValueBytes)
	}
	cut = false
	b := convertValue("LONG VARBINARY", 0, make([]byte, maxValueBytes+10), &cut)
	if raw, _ := b.AsBytes(); !cut || len(raw) != maxValueBytes {
		t.Errorf("long binary: cut=%v len=%d", cut, len(raw))
	}
}

func TestBindValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("", 3600))
	for _, tt := range []struct {
		in   driver.Value
		want any
	}{
		{driver.Null(), nil},
		{driver.Int(3), int64(3)},
		{driver.Float(1.5), 1.5},
		{driver.Bool(true), true},
		{driver.String("x"), "x"},
		{driver.Decimal("12.50"), "12.50"},
		{driver.JSON([]byte(`["a"]`)), `["a"]`},
		{driver.Bytes([]byte{1}), []byte{1}},
		{driver.Time(ts), "2026-01-02T02:04:05Z"},
	} {
		got, err := bindValue(tt.in)
		if err != nil {
			t.Errorf("bindValue(%s): %v", tt.in, err)
			continue
		}
		switch w := tt.want.(type) {
		case []byte:
			if g, ok := got.([]byte); !ok || string(g) != string(w) {
				t.Errorf("bindValue(%s) = %#v, want %#v", tt.in, got, w)
			}
		default:
			if got != tt.want {
				t.Errorf("bindValue(%s) = %#v, want %#v", tt.in, got, tt.want)
			}
		}
	}
	if _, err := bindValue(driver.List(driver.Int(1))); err == nil {
		t.Error("bindValue(list) = nil error, want a refusal")
	}
}
