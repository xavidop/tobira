package mssql

import (
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestColumnType(t *testing.T) {
	for name, want := range map[string]driver.Type{
		"BIT": driver.TypeBool, "TINYINT": driver.TypeInt, "SMALLINT": driver.TypeInt, "INT": driver.TypeInt, "BIGINT": driver.TypeInt,
		"DECIMAL": driver.TypeDecimal, "MONEY": driver.TypeDecimal, "SMALLMONEY": driver.TypeDecimal,
		"FLOAT": driver.TypeFloat, "REAL": driver.TypeFloat,
		"DATE": driver.TypeTime, "DATETIME": driver.TypeTime, "DATETIME2": driver.TypeTime, "SMALLDATETIME": driver.TypeTime, "DATETIMEOFFSET": driver.TypeTime,
		"TIME": driver.TypeString, "CHAR": driver.TypeString, "VARCHAR": driver.TypeString, "NCHAR": driver.TypeString, "NVARCHAR": driver.TypeString,
		"TEXT": driver.TypeString, "NTEXT": driver.TypeString, "XML": driver.TypeString, "UNIQUEIDENTIFIER": driver.TypeString,
		"BINARY": driver.TypeBytes, "VARBINARY": driver.TypeBytes, "IMAGE": driver.TypeBytes,
		"GEOGRAPHY": driver.TypeBytes, "GEOMETRY": driver.TypeBytes, "HIERARCHYID": driver.TypeBytes,
		"SQL_VARIANT": driver.TypeUnknown, "": driver.TypeUnknown,
	} {
		if got := columnType(name); got != want {
			t.Errorf("columnType(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestFieldType(t *testing.T) {
	for name, want := range map[string]driver.Type{
		"bit": driver.TypeBool, "int": driver.TypeInt, "bigint": driver.TypeInt, "numeric": driver.TypeDecimal, "decimal": driver.TypeDecimal,
		"money": driver.TypeDecimal, "float": driver.TypeFloat, "real": driver.TypeFloat, "datetime2": driver.TypeTime, "datetimeoffset": driver.TypeTime,
		"date": driver.TypeTime, "time": driver.TypeString, "nvarchar": driver.TypeString, "sysname": driver.TypeString, "xml": driver.TypeString,
		"uniqueidentifier": driver.TypeString, "varbinary": driver.TypeBytes, "timestamp": driver.TypeBytes, "rowversion": driver.TypeBytes,
		"geography": driver.TypeBytes, "hierarchyid": driver.TypeBytes, "json": driver.TypeJSON, "sql_variant": driver.TypeUnknown,
	} {
		if got := fieldType(name); got != want {
			t.Errorf("fieldType(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestNativeType(t *testing.T) {
	for _, tc := range []struct {
		name             string
		maxLen           int
		precision, scale int
		want             string
	}{
		{"int", 4, 10, 0, "int"},
		{"nvarchar", 510, 0, 0, "nvarchar(255)"},
		{"nvarchar", -1, 0, 0, "nvarchar(max)"},
		{"varchar", 20, 0, 0, "varchar(20)"},
		{"varbinary", -1, 0, 0, "varbinary(max)"},
		{"nchar", 2, 0, 0, "nchar(1)"},
		{"decimal", 9, 10, 2, "decimal(10,2)"},
		{"numeric", 5, 5, 0, "numeric(5,0)"},
		{"datetime2", 8, 27, 7, "datetime2(7)"},
		{"datetimeoffset", 10, 34, 7, "datetimeoffset(7)"},
		{"time", 5, 16, 7, "time(7)"},
		{"float", 8, 53, 0, "float(53)"},
		{"datetime", 8, 23, 3, "datetime"},
		{"xml", -1, 0, 0, "xml"},
	} {
		if got := nativeType(tc.name, tc.maxLen, tc.precision, tc.scale); got != tc.want {
			t.Errorf("nativeType(%q, %d, %d, %d) = %q, want %q", tc.name, tc.maxLen, tc.precision, tc.scale, got, tc.want)
		}
	}
}

func TestConvertValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 600, time.FixedZone("x", 3600))
	guid := []byte{0x67, 0x45, 0x23, 0x01, 0xab, 0x89, 0xef, 0xcd, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	for _, tc := range []struct {
		dbType string
		in     any
		want   driver.Value
	}{
		{"INT", nil, driver.Null()},
		{"INT", int64(7), driver.Int(7)},
		{"BIT", true, driver.Bool(true)},
		{"FLOAT", 1.5, driver.Float(1.5)},
		{"REAL", float64(float32(0.1)), driver.Float(0.1)},
		{"DECIMAL", []byte("12.50"), driver.Decimal("12.50")},
		{"MONEY", []byte("-3.1400"), driver.Decimal("-3.1400")},
		{"DATETIME2", ts, driver.Time(ts.UTC())},
		{"DATETIMEOFFSET", ts, driver.Time(ts.UTC())},
		{"DATE", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))},
		{"TIME", time.Date(1, 1, 1, 13, 4, 5, 120000000, time.UTC), driver.String("13:04:05.12")},
		{"TIME", time.Date(1, 1, 1, 13, 4, 5, 0, time.UTC), driver.String("13:04:05")},
		{"NVARCHAR", "é", driver.String("é")},
		{"XML", "<a/>", driver.String("<a/>")},
		{"UNIQUEIDENTIFIER", guid, driver.String("01234567-89AB-CDEF-0123-456789ABCDEF")},
		{"VARBINARY", []byte{1, 2}, driver.Bytes([]byte{1, 2})},
		{"GEOGRAPHY", []byte{1}, driver.Bytes([]byte{1})},
		{"SQL_VARIANT", int64(3), driver.Int(3)},
		{"SQL_VARIANT", "s", driver.String("s")},
	} {
		got := convertValue(tc.dbType, tc.in)
		if !drivertest.EqualValue(tc.want, got) || got.Type() != tc.want.Type() {
			t.Errorf("convertValue(%s, %#v) = %s (%s), want %s (%s)", tc.dbType, tc.in, got, got.Type(), tc.want, tc.want.Type())
		}
	}
}

func TestToArg(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	for _, tc := range []struct {
		in   driver.Value
		want any
	}{
		{driver.Null(), nil},
		{driver.Bool(true), true},
		{driver.Int(7), int64(7)},
		{driver.Float(1.5), 1.5},
		{driver.Decimal("1.50"), "1.50"},
		{driver.String("é"), "é"},
		{driver.JSON([]byte(`[1]`)), "[1]"},
	} {
		got, err := toArg(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("toArg(%s) = %#v, %v; want %#v", tc.in, got, err, tc.want)
		}
	}
	got, err := toArg(driver.Time(ts))
	if tm, ok := got.(time.Time); err != nil || !ok || !tm.Equal(ts) || tm.Location() != time.UTC {
		t.Errorf("toArg(time) = %#v, %v; want the same instant in UTC", got, err)
	}
	b, err := toArg(driver.Bytes([]byte{1}))
	if bb, ok := b.([]byte); err != nil || !ok || len(bb) != 1 {
		t.Errorf("toArg(bytes) = %#v, %v", b, err)
	}
	if _, err := toArg(driver.List(driver.Int(1))); err == nil {
		t.Error("toArg(list) succeeded, want an error")
	}
}
