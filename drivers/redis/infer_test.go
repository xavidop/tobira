package redis

import (
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestInferType(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want driver.Type
	}{
		{"7", driver.TypeInt}, {"-12", driver.TypeInt}, {"0", driver.TypeInt},
		{"007", driver.TypeString}, {"+7", driver.TypeString}, {"-0", driver.TypeString},
		{"99999999999999999999", driver.TypeString},
		{"12.50", driver.TypeDecimal}, {"-0.5", driver.TypeDecimal}, {"0.0", driver.TypeDecimal},
		{".5", driver.TypeString}, {"5.", driver.TypeString}, {"1e5", driver.TypeString}, {"01.5", driver.TypeString},
		{"true", driver.TypeBool}, {"false", driver.TypeBool}, {"TRUE", driver.TypeString}, {"1 ", driver.TypeString},
		{"2026-01-02T03:04:05Z", driver.TypeTime}, {"2026-01-02T03:04:05.123+02:00", driver.TypeTime},
		{"2026-01-02", driver.TypeString}, {"2026-13-02T03:04:05Z", driver.TypeString},
		{`["a","b"]`, driver.TypeJSON}, {`{"a":1}`, driver.TypeJSON}, {`[]`, driver.TypeJSON},
		{`[unclosed`, driver.TypeString}, {`"quoted"`, driver.TypeString},
		{"", driver.TypeString}, {"hello", driver.TypeString}, {"\xff\xfe", driver.TypeBytes},
	} {
		if got := inferType(tt.in); got != tt.want {
			t.Errorf("inferType(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestUnifyTypes(t *testing.T) {
	for _, tt := range []struct {
		a, b, want driver.Type
	}{
		{driver.TypeNull, driver.TypeInt, driver.TypeInt},
		{driver.TypeInt, driver.TypeNull, driver.TypeInt},
		{driver.TypeInt, driver.TypeInt, driver.TypeInt},
		{driver.TypeInt, driver.TypeDecimal, driver.TypeDecimal},
		{driver.TypeDecimal, driver.TypeInt, driver.TypeDecimal},
		{driver.TypeInt, driver.TypeBool, driver.TypeString},
		{driver.TypeTime, driver.TypeJSON, driver.TypeString},
		{driver.TypeBytes, driver.TypeString, driver.TypeBytes},
		{driver.TypeString, driver.TypeBytes, driver.TypeBytes},
		{driver.TypeInt, driver.TypeBytes, driver.TypeBytes},
	} {
		if got := unifyTypes(tt.a, tt.b); got != tt.want {
			t.Errorf("unifyTypes(%s, %s) = %s, want %s", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestTypedValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tt := range []struct {
		in   string
		typ  driver.Type
		want driver.Value
	}{
		{"7", driver.TypeInt, driver.Int(7)},
		{"7", driver.TypeDecimal, driver.Decimal("7")},
		{"12.50", driver.TypeDecimal, driver.Decimal("12.50")},
		{"true", driver.TypeBool, driver.Bool(true)},
		{"2026-01-02T03:04:05Z", driver.TypeTime, driver.Time(ts)},
		{`["a"]`, driver.TypeJSON, driver.JSON([]byte(`["a"]`))},
		{"7", driver.TypeString, driver.String("7")},
		{"oops", driver.TypeInt, driver.String("oops")}, // a value outside the inferred type stays text
		{"\xff", driver.TypeBytes, driver.Bytes([]byte{0xff})},
		{"ok", driver.TypeBytes, driver.String("ok")},
	} {
		got := typedValue(tt.in, tt.typ)
		if got.Type() != tt.want.Type() || !drivertest.EqualValue(tt.want, got) {
			t.Errorf("typedValue(%q, %s) = %s (%s), want %s (%s)", tt.in, tt.typ, got, got.Type(), tt.want, tt.want.Type())
		}
	}
}

func TestEncodeValue(t *testing.T) {
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		v    driver.Value
		want string
	}{
		{driver.String("x"), "x"},
		{driver.Int(-3), "-3"},
		{driver.Float(1.5), "1.5"},
		{driver.Decimal("1.50"), "1.50"},
		{driver.Bool(true), "true"},
		{driver.Bool(false), "false"},
		{driver.Time(ts), "2026-06-01T12:00:00Z"},
		{driver.JSON([]byte(`[1]`)), "[1]"},
		{driver.Bytes([]byte("b")), "b"},
		{driver.List(driver.String("a"), driver.Int(1)), `["a",1]`},
	} {
		got, err := encodeValue(tt.v)
		if err != nil || got != tt.want {
			t.Errorf("encodeValue(%s) = %q, %v; want %q", tt.v, got, err, tt.want)
		}
		// Round trip: what the driver writes, inference reads back as the same value.
		if tt.v.Type() != driver.TypeFloat && tt.v.Type() != driver.TypeList && tt.v.Type() != driver.TypeBytes {
			back := typedValue(got, inferType(got))
			if !drivertest.EqualValue(tt.v, back) {
				t.Errorf("round trip of %s: read back %s (%s)", tt.v, back, back.Type())
			}
		}
	}
	if _, err := encodeValue(driver.Null()); err == nil {
		t.Error("encodeValue(NULL): want an error")
	}
}

func TestInferRecords(t *testing.T) {
	recs := []map[string]string{
		{"id": "1", "age": "30", "score": "12.50", "active": "true", "note": "007"},
		{"id": "2", "score": "3", "active": "false", "note": "x"},
	}
	fields := inferFields([]string{"id", "age", "score", "active", "note"}, recs)
	want := map[string]driver.Type{"id": driver.TypeInt, "age": driver.TypeInt, "score": driver.TypeDecimal, "active": driver.TypeBool, "note": driver.TypeString}
	if len(fields) != len(want) {
		t.Fatalf("fields = %+v", fields)
	}
	for _, f := range fields {
		if f.Type != want[f.Name] {
			t.Errorf("field %s: type %s, want %s", f.Name, f.Type, want[f.Name])
		}
		if !f.Nullable {
			t.Errorf("field %s: want Nullable (a hash may lack any field)", f.Name)
		}
	}
}

func TestPreviewString(t *testing.T) {
	// A cut in the middle of a rune keeps a text preview with the marker.
	got := previewString("ab\xc3", true)
	if s, ok := got.AsText(); !ok || s != "ab…" {
		t.Errorf("previewString(cut rune) = %s (%s), want ab…", got, got.Type())
	}
	if s, _ := previewString("abc", false).AsText(); s != "abc" {
		t.Errorf("uncut = %q", s)
	}
	if got := previewString("\xff\xfe", true); got.Type() != driver.TypeBytes {
		t.Errorf("binary preview = %s", got.Type())
	}
}

func TestCanonicalType(t *testing.T) {
	for in, want := range map[string]string{
		"List": "list", "LIST": "list", "Hash": "hash", "json": "ReJSON-RL", "JSON": "ReJSON-RL",
		"rejson-rl": "ReJSON-RL", "ReJSON-RL": "ReJSON-RL", "ZSet": "zset", "String": "string", "": "",
	} {
		if got := canonicalType(in); got != want {
			t.Errorf("canonicalType(%q) = %q, want %q", in, got, want)
		}
	}
	ch, err := splitChange("update", driver.Object{{Key: "type", Value: driver.String("Set")}})
	if err != nil || ch.typ != "set" {
		t.Errorf("splitChange type = %q, %v", ch.typ, err)
	}
}
