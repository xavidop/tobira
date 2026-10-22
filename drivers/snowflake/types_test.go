package snowflake

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestColumnType(t *testing.T) {
	for _, c := range []struct {
		native string
		scale  int64
		want   driver.Type
	}{
		{"FIXED", 0, driver.TypeInt},
		{"FIXED", 2, driver.TypeDecimal},
		{"NUMBER", 0, driver.TypeInt},
		{"NUMBER", 4, driver.TypeDecimal},
		{"DECFLOAT", 0, driver.TypeDecimal},
		{"REAL", 0, driver.TypeFloat},
		{"FLOAT", 0, driver.TypeFloat},
		{"TEXT", 0, driver.TypeString},
		{"VARCHAR", 0, driver.TypeString},
		{"BOOLEAN", 0, driver.TypeBool},
		{"DATE", 0, driver.TypeTime},
		{"TIMESTAMP_NTZ", 0, driver.TypeTime},
		{"TIMESTAMP_LTZ", 0, driver.TypeTime},
		{"TIMESTAMP_TZ", 0, driver.TypeTime},
		{"TIME", 0, driver.TypeString},
		{"VARIANT", 0, driver.TypeJSON},
		{"OBJECT", 0, driver.TypeJSON},
		{"ARRAY", 0, driver.TypeJSON},
		{"MAP", 0, driver.TypeJSON},
		{"BINARY", 0, driver.TypeBytes},
		{"GEOGRAPHY", 0, driver.TypeString},
		{"GEOMETRY", 0, driver.TypeString},
		{"VECTOR", 0, driver.TypeVector},
		{"SOMETHING_NEW", 0, driver.TypeUnknown},
		{"number", 0, driver.TypeInt}, // case-insensitive
	} {
		if got := columnType(c.native, c.scale); got != c.want {
			t.Errorf("columnType(%q, %d) = %v, want %v", c.native, c.scale, got, c.want)
		}
	}
}

func TestConvertValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	tz := time.Date(2026, 1, 2, 5, 4, 5, 0, time.FixedZone("", 2*3600))
	for _, c := range []struct {
		name   string
		native string
		scale  int64
		raw    any
		want   driver.Value
	}{
		{"null", "FIXED", 0, nil, driver.Null()},
		{"int", "FIXED", 0, "42", driver.Int(42)},
		{"negative int", "FIXED", 0, "-7", driver.Int(-7)},
		{"int past int64", "FIXED", 0, "12345678901234567890", driver.Decimal("12345678901234567890")},
		{"decimal keeps trailing zeros", "FIXED", 2, "12.50", driver.Decimal("12.50")},
		{"decimal from float64 (JSON results)", "FIXED", 2, 1.5, driver.Decimal("1.50")},
		{"int64", "FIXED", 0, int64(9), driver.Int(9)},
		{"decfloat", "DECFLOAT", 0, "1.234E+40", driver.Decimal("1.234E+40")},
		{"real", "REAL", 0, 2.5, driver.Float(2.5)},
		{"text", "TEXT", 0, "héllo", driver.String("héllo")},
		{"bool", "BOOLEAN", 0, true, driver.Bool(true)},
		{"ntz", "TIMESTAMP_NTZ", 0, ts, driver.Time(ts)},
		{"tz keeps instant", "TIMESTAMP_TZ", 0, tz, driver.Time(tz)},
		{"date", "DATE", 0, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))},
		{"time of day", "TIME", 0, time.Date(1, 1, 1, 3, 4, 5, 500000000, time.UTC), driver.String("03:04:05.5")},
		{"time whole seconds", "TIME", 0, time.Date(1, 1, 1, 23, 0, 0, 0, time.UTC), driver.String("23:00:00")},
		{"variant json", "VARIANT", 0, `{"a":[1,2]}`, driver.JSON([]byte(`{"a":[1,2]}`))},
		{"array json", "ARRAY", 0, "[\n  \"x\"\n]", driver.JSON([]byte("[\n  \"x\"\n]"))},
		{"variant not json", "VARIANT", 0, "not json", driver.String("not json")},
		{"binary", "BINARY", 0, []byte{1, 2}, driver.Bytes([]byte{1, 2})},
		{"geography", "GEOGRAPHY", 0, `{"type":"Point"}`, driver.String(`{"type":"Point"}`)},
		{"vector float", "VECTOR", 0, []float32{1, 2}, driver.Vector([]float32{1, 2})},
		{"vector int", "VECTOR", 0, []int32{1, 2}, driver.List(driver.Int(1), driver.Int(2))},
		{"unexpected go type", "TEXT", 0, struct{}{}, driver.Unknown("TEXT", []byte("{}"), "{}")},
	} {
		var cut bool
		got := convertValue(c.native, c.scale, c.raw, &cut)
		if !drivertest.EqualValue(c.want, got) || got.Type() != c.want.Type() || got.String() != c.want.String() {
			t.Errorf("%s: convertValue(%q, %d, %#v) = %#v (%v), want %#v (%v)", c.name, c.native, c.scale, c.raw, got, got.Type(), c.want, c.want.Type())
		}
		if cut {
			t.Errorf("%s: a small value was reported cut", c.name)
		}
	}
}

// A VARIANT, string or binary value above the cap is cut and reported, so
// one huge cell cannot hold the whole result in memory twice over.
func TestConvertValueCaps(t *testing.T) {
	big := strings.Repeat("é", maxValueBytes) // two bytes each
	var cut bool
	v := convertValue("TEXT", 0, big, &cut)
	s, _ := v.AsText()
	if !cut || len(s) > maxValueBytes || !strings.HasPrefix(big, s) || !utf8.ValidString(s) {
		t.Errorf("TEXT over the cap: cut=%v len=%d", cut, len(s))
	}

	cut = false
	v = convertValue("VARIANT", 0, `["`+strings.Repeat("x", maxValueBytes)+`"]`, &cut)
	if !cut || v.Type() != driver.TypeString {
		t.Errorf("VARIANT over the cap: cut=%v type=%v, want a cut String", cut, v.Type())
	}

	cut = false
	v = convertValue("BINARY", 0, make([]byte, maxValueBytes+10), &cut)
	b, _ := v.AsBytes()
	if !cut || len(b) != maxValueBytes {
		t.Errorf("BINARY over the cap: cut=%v len=%d", cut, len(b))
	}
}

func TestBindValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("", 3600))
	for _, c := range []struct {
		in   driver.Value
		want any
	}{
		{driver.Null(), nil},
		{driver.Int(7), int64(7)},
		{driver.Float(1.5), 1.5},
		{driver.Bool(true), true},
		{driver.String("x"), "x"},
		{driver.Decimal("12.50"), "12.50"},
		{driver.Time(ts), "2026-01-02T02:04:05Z"},
		{driver.JSON([]byte(`[1]`)), `[1]`},
		{driver.Bytes([]byte{1}), []byte{1}},
	} {
		got, err := bindValue(c.in)
		if err != nil {
			t.Errorf("bindValue(%v): %v", c.in, err)
			continue
		}
		if b, ok := c.want.([]byte); ok {
			if gb, ok := got.([]byte); !ok || string(gb) != string(b) {
				t.Errorf("bindValue(%v) = %#v, want %#v", c.in, got, c.want)
			}
			continue
		}
		if got != c.want {
			t.Errorf("bindValue(%v) = %#v, want %#v", c.in, got, c.want)
		}
	}
	if _, err := bindValue(driver.List(driver.Int(1))); err == nil {
		t.Error("bindValue(List) = nil error, want one")
	}
}

// Snowflake lists views in INFORMATION_SCHEMA.TABLES with their own
// table_type.
func TestCollectionType(t *testing.T) {
	for in, want := range map[string]driver.CollectionType{
		"BASE TABLE": driver.CollectionTable, "VIEW": driver.CollectionView, "MATERIALIZED VIEW": driver.CollectionMaterializedView,
		"EXTERNAL TABLE": driver.CollectionTable, "TEMPORARY TABLE": driver.CollectionTable, "EVENT TABLE": driver.CollectionTable,
		"LOCAL TEMPORARY": driver.CollectionTable, "SOMETHING": driver.CollectionOther,
	} {
		if got := collectionType(in); got != want {
			t.Errorf("collectionType(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTimeTextPerNativeType(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("", 2*3600))
	for native, want := range map[string]string{
		"TIMESTAMP_TZ":    "2026-01-02T03:04:05+02:00",
		"TIMESTAMP_TZ(9)": "2026-01-02T03:04:05+02:00",
		"TIMESTAMP_LTZ":   "2026-01-02T01:04:05Z",
		"TIMESTAMP_NTZ":   "2026-01-02 01:04:05",
		"DATE":            "2026-01-02",
		"TIME":            "01:04:05",
		"NUMBER(38,0)":    "2026-01-02T01:04:05Z",
	} {
		if got := timeText(native, ts); got != want {
			t.Errorf("timeText(%q) = %q, want %q", native, got, want)
		}
	}
}
