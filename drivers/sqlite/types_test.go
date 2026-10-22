package sqlite

import (
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestTypeMapping(t *testing.T) {
	tests := []struct {
		declared string
		want     driver.Type
	}{
		{"INTEGER", driver.TypeInt}, {"INT", driver.TypeInt}, {"BIGINT", driver.TypeInt}, {"smallint", driver.TypeInt},
		{"REAL", driver.TypeFloat}, {"DOUBLE", driver.TypeFloat}, {"FLOAT", driver.TypeFloat},
		{"DECIMAL(10,2)", driver.TypeDecimal}, {"NUMERIC", driver.TypeDecimal},
		{"BOOLEAN", driver.TypeBool}, {"BOOL", driver.TypeBool},
		{"DATE", driver.TypeTime}, {"DATETIME", driver.TypeTime}, {"TIMESTAMP", driver.TypeTime},
		{"JSON", driver.TypeJSON}, {"BLOB", driver.TypeBytes},
		{"TEXT", driver.TypeString}, {"VARCHAR(20)", driver.TypeString}, {"CHAR(3)", driver.TypeString}, {"", driver.TypeString},
	}
	for _, tt := range tests {
		if got := mapType(tt.declared); got != tt.want {
			t.Errorf("mapType(%q) = %v, want %v", tt.declared, got, tt.want)
		}
	}
}

func TestConvertValue(t *testing.T) {
	utc := func(s string) driver.Value {
		tm, err := time.Parse("2006-01-02 15:04:05", s)
		if err != nil {
			t.Fatal(err)
		}
		return driver.Time(tm)
	}
	tests := []struct {
		name     string
		declared string
		in       any
		want     driver.Value
	}{
		{"null", "INTEGER", nil, driver.Null()},
		{"null bool", "BOOLEAN", nil, driver.Null()},
		{"int", "INTEGER", int64(7), driver.Int(7)},
		{"float", "REAL", 1.5, driver.Float(1.5)},
		{"bool true", "BOOLEAN", int64(1), driver.Bool(true)},
		{"bool false", "BOOL", int64(0), driver.Bool(false)},
		{"bool other", "BOOLEAN", int64(5), driver.Int(5)},
		{"decimal float", "DECIMAL(10,2)", 12.5, driver.Decimal("12.50")},
		{"decimal int", "DECIMAL(10,2)", int64(7), driver.Decimal("7.00")},
		{"decimal text", "NUMERIC(8,3)", "3.14", driver.Decimal("3.140")},
		{"decimal text long", "NUMERIC(8,1)", "3.14", driver.Decimal("3.14")},
		{"time no zone T", "DATETIME", "2026-10-19T09:12:00", utc("2026-10-19 09:12:00")},
		{"decimal no scale", "NUMERIC", 12.5, driver.Decimal("12.5")},
		{"time text", "DATETIME", "2026-10-19 09:12:00", utc("2026-10-19 09:12:00")},
		{"time rfc3339", "TIMESTAMP", "2026-10-19T09:12:00Z", utc("2026-10-19 09:12:00")},
		{"date", "DATE", "2026-10-19", utc("2026-10-19 00:00:00")},
		{"time unix", "DATETIME", int64(0), driver.Time(time.Unix(0, 0).UTC())},
		{"time bad", "DATETIME", "yesterday", driver.String("yesterday")},
		{"json ok", "JSON", `{"a":1}`, driver.JSON([]byte(`{"a":1}`))},
		{"json bad", "JSON", `{oops`, driver.String(`{oops`)},
		{"blob", "BLOB", []byte{1, 2}, driver.Bytes([]byte{1, 2})},
		{"text", "VARCHAR(20)", "hi", driver.String("hi")},
		{"empty text bytes", "TEXT", []byte("hi"), driver.String("hi")},
		{"expr int", "", int64(3), driver.Int(3)},
		{"expr float", "", 2.5, driver.Float(2.5)},
		{"expr bytes", "", []byte{9}, driver.Bytes([]byte{9})},
		{"expr string", "", "x", driver.String("x")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertValue(tt.declared, tt.in)
			if got.Type() != tt.want.Type() || got.String() != tt.want.String() {
				t.Errorf("convertValue(%q, %v) = %v (%v), want %v (%v)", tt.declared, tt.in, got, got.Type(), tt.want, tt.want.Type())
			}
		})
	}
}

func TestColumnType(t *testing.T) {
	if got := columnType("", int64(1)); got != driver.TypeInt {
		t.Errorf("expr type = %v", got)
	}
}
