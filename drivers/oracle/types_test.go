package oracle

import (
	"database/sql"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	go_ora "github.com/sijms/go-ora/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestResultColumn(t *testing.T) {
	for _, tc := range []struct {
		db     string
		p, s   int64
		ps     bool
		typ    driver.Type
		native string
	}{
		{"NUMBER", 10, 0, true, driver.TypeInt, "NUMBER(10)"},
		{"NUMBER", 18, 0, true, driver.TypeInt, "NUMBER(18)"},
		{"NUMBER", 19, 0, true, driver.TypeDecimal, "NUMBER(19)"},
		{"NUMBER", 10, 2, true, driver.TypeDecimal, "NUMBER(10,2)"},
		{"NUMBER", 38, 255, true, driver.TypeDecimal, "NUMBER"},
		{"IBFloat", 0, 0, false, driver.TypeFloat, "BINARY_FLOAT"},
		{"IBDouble", 0, 0, false, driver.TypeFloat, "BINARY_DOUBLE"},
		{"NCHAR", 0, 0, false, driver.TypeString, "VARCHAR2"},
		{"CHAR", 0, 0, false, driver.TypeString, "CHAR"},
		{"LongVarChar", 0, 0, false, driver.TypeString, "CLOB"},
		{"LONG", 0, 0, false, driver.TypeString, "LONG"},
		{"RAW", 0, 0, false, driver.TypeBytes, "RAW"},
		{"LongRaw", 0, 0, false, driver.TypeBytes, "BLOB"},
		{"OCIBlobLocator", 0, 0, false, driver.TypeJSON, "JSON"},
		{"DATE", 0, 0, false, driver.TypeTime, "DATE"},
		{"TimeStampDTY", 0, 0, false, driver.TypeTime, "TIMESTAMP"},
		{"TimeStampTZ_DTY", 0, 0, false, driver.TypeTime, "TIMESTAMP WITH TIME ZONE"},
		{"TimeStampLTZ_DTY", 0, 0, false, driver.TypeTime, "TIMESTAMP WITH LOCAL TIME ZONE"},
		{"IntervalDS_DTY", 0, 0, false, driver.TypeString, "INTERVAL DAY TO SECOND"},
		{"IntervalYM_DTY", 0, 0, false, driver.TypeString, "INTERVAL YEAR TO MONTH"},
		{"ROWID", 0, 0, false, driver.TypeString, "ROWID"},
		{"TNSType(127)", 0, 0, false, driver.TypeVector, "VECTOR"},
		{"XMLType", 0, 0, false, driver.TypeUnknown, "XMLType"},
	} {
		ci := resultColumn(tc.db, tc.p, tc.s, tc.ps)
		if ci.typ != tc.typ || ci.native != tc.native {
			t.Errorf("resultColumn(%s, %d, %d) = %s %q, want %s %q", tc.db, tc.p, tc.s, ci.typ, ci.native, tc.typ, tc.native)
		}
	}
}

func TestFieldType(t *testing.T) {
	n := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	none := sql.NullInt64{}
	for _, tc := range []struct {
		dt   string
		p, s sql.NullInt64
		want driver.Type
	}{
		{"NUMBER", n(10), n(0), driver.TypeInt},
		{"NUMBER", n(1), n(0), driver.TypeInt},
		{"NUMBER", n(19), n(0), driver.TypeDecimal},
		{"NUMBER", n(10), n(2), driver.TypeDecimal},
		{"NUMBER", none, none, driver.TypeDecimal},
		{"NUMBER", none, n(0), driver.TypeInt}, // INTEGER
		{"FLOAT", n(126), none, driver.TypeDecimal},
		{"BINARY_FLOAT", none, none, driver.TypeFloat},
		{"BINARY_DOUBLE", none, none, driver.TypeFloat},
		{"VARCHAR2", none, none, driver.TypeString},
		{"NVARCHAR2", none, none, driver.TypeString},
		{"CHAR", none, none, driver.TypeString},
		{"NCHAR", none, none, driver.TypeString},
		{"CLOB", none, none, driver.TypeString},
		{"NCLOB", none, none, driver.TypeString},
		{"LONG", none, none, driver.TypeString},
		{"ROWID", none, none, driver.TypeString},
		{"UROWID", none, none, driver.TypeString},
		{"INTERVAL DAY(2) TO SECOND(6)", none, none, driver.TypeString},
		{"INTERVAL YEAR(2) TO MONTH", none, none, driver.TypeString},
		{"RAW", none, none, driver.TypeBytes},
		{"LONG RAW", none, none, driver.TypeBytes},
		{"BLOB", none, none, driver.TypeBytes},
		{"DATE", none, none, driver.TypeTime},
		{"TIMESTAMP(6)", none, none, driver.TypeTime},
		{"TIMESTAMP(6) WITH TIME ZONE", none, none, driver.TypeTime},
		{"TIMESTAMP(9) WITH LOCAL TIME ZONE", none, none, driver.TypeTime},
		{"BOOLEAN", none, none, driver.TypeBool},
		{"JSON", none, none, driver.TypeJSON},
		{"VECTOR", none, none, driver.TypeVector},
		{"BFILE", none, none, driver.TypeUnknown},
		{"XMLTYPE", none, none, driver.TypeUnknown},
		{"SDO_GEOMETRY", none, none, driver.TypeUnknown},
	} {
		if got := fieldType(tc.dt, tc.p, tc.s); got != tc.want {
			t.Errorf("fieldType(%q, %v, %v) = %s, want %s", tc.dt, tc.p, tc.s, got, tc.want)
		}
	}
}

func TestConvertValue(t *testing.T) {
	intCol := resultColumn("NUMBER", 10, 0, true)
	decCol := resultColumn("NUMBER", 10, 2, true)
	anyNum := resultColumn("NUMBER", 38, 255, true)
	str := resultColumn("NCHAR", 0, 0, false)
	tz := resultColumn("TimeStampTZ_DTY", 0, 0, false)
	js := resultColumn("OCIBlobLocator", 0, 0, false)
	raw := resultColumn("RAW", 0, 0, false)
	vec := resultColumn("TNSType(127)", 0, 0, false)
	f32 := resultColumn("IBFloat", 0, 0, false)
	plus2 := time.FixedZone("+02:00", 2*3600)
	for _, tc := range []struct {
		name string
		col  colInfo
		in   any
		want driver.Value
	}{
		{"null", intCol, nil, driver.Null()},
		{"int", intCol, "42", driver.Int(42)},
		{"negative int", intCol, "-7", driver.Int(-7)},
		{"decimal keeps its scale", decCol, "12.5", driver.Decimal("12.50")},
		{"decimal integer", decCol, "3", driver.Decimal("3.00")},
		{"negative decimal", decCol, "-0.5", driver.Decimal("-0.50")},
		{"unconstrained integral is Int", anyNum, "1", driver.Int(1)},
		{"unconstrained fraction is Decimal", anyNum, "3.14", driver.Decimal("3.14")},
		{"unconstrained huge is Decimal", anyNum, "123456789012345678901234567890", driver.Decimal("123456789012345678901234567890")},
		{"float64 number", anyNum, float64(2.5), driver.Float(2.5)},
		{"int64 number", intCol, int64(9), driver.Int(9)},
		{"string", str, "hi", driver.String("hi")},
		{"time keeps its instant", tz, time.Date(2026, 1, 2, 3, 4, 5, 0, plus2), driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 0, plus2))},
		{"json bytes", js, []byte(`["a"]`), driver.JSON([]byte(`["a"]`))},
		{"invalid json bytes", js, []byte{0xff, 0x01}, driver.Bytes([]byte{0xff, 0x01})},
		{"raw", raw, []byte{1, 2}, driver.Bytes([]byte{1, 2})},
		{"vector float32", vec, []float32{1, 2}, driver.Vector([]float32{1, 2})},
		{"vector float64", vec, []float64{1.5, 2}, driver.Vector([]float32{1.5, 2})},
		{"float32 shortest", f32, float32(0.1), driver.Float(0.1)},
		{"bool", str, true, driver.Bool(true)},
	} {
		got, trunc := convertValue(tc.col, tc.in)
		if trunc {
			t.Errorf("%s: unexpected truncation", tc.name)
		}
		if got.Type() != tc.want.Type() || !drivertest.EqualValue(tc.want, got) || got.String() != tc.want.String() {
			t.Errorf("%s: convertValue(%#v) = %s (%s), want %s (%s)", tc.name, tc.in, got, got.Type(), tc.want, tc.want.Type())
		}
	}
}

// TIMESTAMP WITH TIME ZONE keeps the zone go-ora reports (a fixed offset or
// a region); the other time types are UTC.
func TestConvertValueTimeZones(t *testing.T) {
	plus2 := time.FixedZone("+02:00", 2*3600)
	in := time.Date(2026, 1, 2, 3, 4, 5, 0, plus2)
	v, _ := convertValue(resultColumn("TimeStampTZ_DTY", 0, 0, false), in)
	got, _ := v.AsTime()
	if !got.Equal(in) || got.Location().String() != "+02:00" {
		t.Errorf("TIMESTAMP WITH TIME ZONE: %v, want %v in +02:00", got, in)
	}
	if berlin, err := time.LoadLocation("Europe/Berlin"); err == nil {
		v, _ = convertValue(resultColumn("TimeStampTZ_DTY", 0, 0, false), in.In(berlin))
		if got, _ := v.AsTime(); got.Location().String() != "Europe/Berlin" {
			t.Errorf("region zone lost: %v", got.Location())
		}
	}
	for _, db := range []string{"TimeStampLTZ_DTY", "TimeStampDTY", "DATE"} {
		v, _ = convertValue(resultColumn(db, 0, 0, false), in)
		if got, _ := v.AsTime(); got.Location() != time.UTC || !got.Equal(in) {
			t.Errorf("%s: %v, want UTC", db, got)
		}
	}
}

func TestConvertValueCapsLOBs(t *testing.T) {
	clob := resultColumn("LongVarChar", 0, 0, false)
	blob := resultColumn("LongRaw", 0, 0, false)
	js := resultColumn("OCIBlobLocator", 0, 0, false)
	big := strings.Repeat("é", maxLOBBytes) // 2 bytes each: twice the cap
	v, trunc := convertValue(clob, big)
	s, _ := v.AsText()
	if !trunc || len(s) > maxLOBBytes || !utf8.ValidString(s) || v.Type() != driver.TypeString {
		t.Errorf("CLOB over the cap: truncated=%v len=%d valid=%v type=%s", trunc, len(s), utf8.ValidString(s), v.Type())
	}
	v, trunc = convertValue(blob, make([]byte, maxLOBBytes+1))
	b, _ := v.AsBytes()
	if !trunc || len(b) != maxLOBBytes {
		t.Errorf("BLOB over the cap: truncated=%v len=%d", trunc, len(b))
	}
	v, trunc = convertValue(js, []byte(`"`+strings.Repeat("x", maxLOBBytes)+`"`))
	if !trunc || v.Type() != driver.TypeString {
		t.Errorf("JSON over the cap: truncated=%v type=%s, want a truncated String", trunc, v.Type())
	}
	v, trunc = convertValue(clob, "short")
	if trunc || v.String() != "short" {
		t.Errorf("short CLOB: %s truncated=%v", v, trunc)
	}
	// Non-LOB strings are not capped (VARCHAR2 is at most 32767 bytes).
	if _, trunc := convertValue(resultColumn("NCHAR", 0, 0, false), strings.Repeat("x", maxLOBBytes+1)); trunc {
		t.Error("a VARCHAR2 value must not be capped")
	}
}

func TestToArg(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	for _, tc := range []struct {
		in    driver.Value
		check func(any) bool
	}{
		{driver.Null(), func(a any) bool { return a == nil }},
		{driver.Bool(true), func(a any) bool { return a == int64(1) }},
		{driver.Bool(false), func(a any) bool { return a == int64(0) }},
		{driver.Int(5), func(a any) bool { return a == int64(5) }},
		{driver.Float(1.5), func(a any) bool { return a == 1.5 }},
		{driver.String("x"), func(a any) bool { return a == "x" }},
		{driver.Decimal("12.50"), func(a any) bool { n, ok := a.(*go_ora.Number); return ok && n != nil }},
		{driver.Bytes([]byte{1}), func(a any) bool { b, ok := a.([]byte); return ok && len(b) == 1 }},
		{driver.Time(ts), func(a any) bool { x, ok := a.(time.Time); return ok && x.Equal(ts) && x.Location() == time.UTC }},
		{driver.JSON([]byte(`[1]`)), func(a any) bool { return a == "[1]" }},
	} {
		got, err := toArg(tc.in)
		if err != nil || !tc.check(got) {
			t.Errorf("toArg(%s %s) = %#v, %v", tc.in, tc.in.Type(), got, err)
		}
	}
	for _, v := range []driver.Value{driver.List(driver.Int(1)), driver.ObjectValue(), driver.Decimal("abc"), driver.Vector([]float32{1})} {
		if _, err := toArg(v); err == nil {
			t.Errorf("toArg(%s %s): want an error", v, v.Type())
		}
	}
}
