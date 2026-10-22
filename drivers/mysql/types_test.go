package mysql

import (
	"math"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestColumnType(t *testing.T) {
	tests := []struct {
		name string
		want driver.Type
	}{
		{"TINYINT", driver.TypeInt},
		{"UNSIGNED TINYINT", driver.TypeInt},
		{"SMALLINT", driver.TypeInt},
		{"MEDIUMINT", driver.TypeInt},
		{"INT", driver.TypeInt},
		{"UNSIGNED INT", driver.TypeInt},
		{"BIGINT", driver.TypeInt},
		{"UNSIGNED BIGINT", driver.TypeInt},
		{"YEAR", driver.TypeInt},
		{"BIT", driver.TypeInt},
		{"DECIMAL", driver.TypeDecimal},
		{"FLOAT", driver.TypeFloat},
		{"DOUBLE", driver.TypeFloat},
		{"DATE", driver.TypeTime},
		{"DATETIME", driver.TypeTime},
		{"TIMESTAMP", driver.TypeTime},
		{"TIME", driver.TypeString},
		{"JSON", driver.TypeJSON},
		{"CHAR", driver.TypeString},
		{"VARCHAR", driver.TypeString},
		{"TEXT", driver.TypeString},
		{"TINYTEXT", driver.TypeString},
		{"MEDIUMTEXT", driver.TypeString},
		{"LONGTEXT", driver.TypeString},
		{"ENUM", driver.TypeString},
		{"SET", driver.TypeString},
		{"BINARY", driver.TypeBytes},
		{"VARBINARY", driver.TypeBytes},
		{"BLOB", driver.TypeBytes},
		{"TINYBLOB", driver.TypeBytes},
		{"MEDIUMBLOB", driver.TypeBytes},
		{"LONGBLOB", driver.TypeBytes},
		{"GEOMETRY", driver.TypeBytes},
		{"NULL", driver.TypeUnknown},
		{"", driver.TypeUnknown},
	}
	for _, tt := range tests {
		if got := columnType(tt.name); got != tt.want {
			t.Errorf("columnType(%q) = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestFieldType(t *testing.T) {
	tests := []struct {
		dataType, columnType string
		want                 driver.Type
	}{
		{"tinyint", "tinyint(1)", driver.TypeBool},
		{"tinyint", "tinyint(1) unsigned", driver.TypeBool},
		{"tinyint", "tinyint", driver.TypeInt},
		{"tinyint", "tinyint(4)", driver.TypeInt},
		{"int", "int", driver.TypeInt},
		{"bigint", "bigint unsigned", driver.TypeInt},
		{"decimal", "decimal(10,2)", driver.TypeDecimal},
		{"float", "float", driver.TypeFloat},
		{"double", "double", driver.TypeFloat},
		{"datetime", "datetime(6)", driver.TypeTime},
		{"timestamp", "timestamp", driver.TypeTime},
		{"date", "date", driver.TypeTime},
		{"time", "time", driver.TypeString},
		{"year", "year", driver.TypeInt},
		{"bit", "bit(8)", driver.TypeInt},
		{"json", "json", driver.TypeJSON},
		{"varchar", "varchar(255)", driver.TypeString},
		{"enum", "enum('a','b')", driver.TypeString},
		{"blob", "blob", driver.TypeBytes},
		{"varbinary", "varbinary(16)", driver.TypeBytes},
		{"geometry", "geometry", driver.TypeBytes},
		{"vector", "vector(3)", driver.TypeUnknown},
	}
	for _, tt := range tests {
		if got := fieldType(tt.dataType, tt.columnType); got != tt.want {
			t.Errorf("fieldType(%q, %q) = %s, want %s", tt.dataType, tt.columnType, got, tt.want)
		}
	}
}

func TestConvertValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name   string
		dbType string
		in     any
		want   driver.Value
	}{
		{"null", "INT", nil, driver.Null()},
		{"int", "INT", int64(5), driver.Int(5)},
		{"tinyint stays int", "TINYINT", int64(1), driver.Int(1)},
		{"unsigned small", "UNSIGNED BIGINT", uint64(5), driver.Int(5)},
		{"unsigned max int64", "UNSIGNED BIGINT", uint64(math.MaxInt64), driver.Int(math.MaxInt64)},
		{"unsigned above int64", "UNSIGNED BIGINT", uint64(math.MaxUint64), driver.Decimal("18446744073709551615")},
		{"unsigned text above int64", "UNSIGNED BIGINT", []byte("18446744073709551615"), driver.Decimal("18446744073709551615")},
		{"int as text", "INT", []byte("-12"), driver.Int(-12)},
		{"decimal", "DECIMAL", []byte("12.50"), driver.Decimal("12.50")},
		{"float32", "FLOAT", float32(0.1), driver.Float(0.1)},
		{"double", "DOUBLE", 2.5, driver.Float(2.5)},
		{"double as text", "DOUBLE", []byte("2.5"), driver.Float(2.5)},
		{"datetime", "DATETIME", ts, driver.Time(ts)},
		{"timestamp local becomes utc", "TIMESTAMP", ts.In(time.FixedZone("x", 3600)), driver.Time(ts)},
		{"json", "JSON", []byte(`["a"]`), driver.JSON([]byte(`["a"]`))},
		{"blob", "BLOB", []byte{0, 1}, driver.Bytes([]byte{0, 1})},
		{"varbinary", "VARBINARY", []byte("ab"), driver.Bytes([]byte("ab"))},
		{"varchar", "VARCHAR", []byte("hé"), driver.String("hé")},
		{"text", "TEXT", []byte("t"), driver.String("t")},
		{"time of day", "TIME", []byte("838:59:59"), driver.String("838:59:59")},
		{"year", "YEAR", int64(2026), driver.Int(2026)},
		{"bit", "BIT", []byte{0x01, 0x02}, driver.Int(258)},
		{"bit 64 above int64", "BIT", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, driver.Decimal("18446744073709551615")},
		{"enum", "ENUM", []byte("a"), driver.String("a")},
		{"string value", "VARCHAR", "s", driver.String("s")},
		{"unknown type", "VECTOR", []byte{1}, driver.Unknown("VECTOR", []byte{1}, "\x01")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertValue(tt.dbType, tt.in)
			if got.Type() != tt.want.Type() || !drivertest.EqualValue(tt.want, got) {
				t.Errorf("convertValue(%q, %#v) = %s (%s), want %s (%s)", tt.dbType, tt.in, got, got.Type(), tt.want, tt.want.Type())
			}
			if tm, ok := got.AsTime(); ok && tm.Location() != time.UTC {
				t.Errorf("time not in UTC: %v", tm)
			}
		})
	}
}

func TestToArg(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	tests := []struct {
		in   driver.Value
		want any
	}{
		{driver.Null(), nil},
		{driver.Bool(true), true},
		{driver.Int(7), int64(7)},
		{driver.Float(1.5), 1.5},
		{driver.Decimal("1.25"), "1.25"},
		{driver.String("s"), "s"},
		{driver.Bytes([]byte("b")), []byte("b")},
		{driver.JSON([]byte(`{"a":1}`)), `{"a":1}`},
	}
	for _, tt := range tests {
		got, err := toArg(tt.in)
		if err != nil {
			t.Errorf("toArg(%s): %v", tt.in, err)
			continue
		}
		switch w := tt.want.(type) {
		case []byte:
			if g, ok := got.([]byte); !ok || string(g) != string(w) {
				t.Errorf("toArg(%s) = %#v, want %#v", tt.in, got, w)
			}
		default:
			if got != tt.want {
				t.Errorf("toArg(%s) = %#v, want %#v", tt.in, got, tt.want)
			}
		}
	}
	got, err := toArg(driver.Time(ts))
	if tm, ok := got.(time.Time); err != nil || !ok || !tm.Equal(ts) || tm.Location() != time.UTC {
		t.Errorf("toArg(time) = %#v, %v; want the instant in UTC", got, err)
	}
	if _, err := toArg(driver.Vector([]float32{1})); err == nil {
		t.Error("toArg(vector) succeeded, want an error")
	}
}
