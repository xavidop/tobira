package firestore

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/genproto/googleapis/type/latlng"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestValueOf(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.FixedZone("x", 3600))
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	tests := []struct {
		in     any
		want   driver.Value
		native string
	}{
		{nil, driver.Null(), "null"},
		{true, driver.Bool(true), "boolean"},
		{int64(-7), driver.Int(-7), "integer"},
		{2.5, driver.Float(2.5), "double"},
		{"hi", driver.String("hi"), "string"},
		{[]byte{1, 2}, driver.Bytes([]byte{1, 2}), "bytes"},
		{ts, driver.Time(ts.UTC()), "timestamp"},
		{&latlng.LatLng{Latitude: 1.5, Longitude: -2}, driver.ObjectValue(m("latitude", driver.Float(1.5)), m("longitude", driver.Float(-2))), "geopoint"},
		{[]any{int64(1), "a", nil}, driver.List(driver.Int(1), driver.String("a"), driver.Null()), "array"},
		{map[string]any{"b": int64(2), "a": map[string]any{"z": true}}, driver.ObjectValue(m("a", driver.ObjectValue(m("z", driver.Bool(true)))), m("b", driver.Int(2))), "map"},
		{fs.Vector64{0.5, 1}, driver.Vector([]float32{0.5, 1}), "vector"},
		{fs.BSONInt32(5), driver.Int(5), "int32"},
		{fs.BSONDecimal128("1.25"), driver.Decimal("1.25"), "decimal128"},
	}
	for _, tt := range tests {
		got := valueOf(tt.in)
		if !drivertest.EqualValue(tt.want, got) || got.Type() != tt.want.Type() {
			t.Errorf("valueOf(%#v) = %s (%s), want %s (%s)", tt.in, got, got.Type(), tt.want, tt.want.Type())
		}
		if n := nativeType(tt.in); n != tt.native {
			t.Errorf("nativeType(%#v) = %q, want %q", tt.in, n, tt.native)
		}
	}
	// Time keeps microseconds and is UTC.
	if got, _ := valueOf(ts).AsTime(); !got.Equal(ts) || got.Location() != time.UTC {
		t.Errorf("time = %v", got)
	}
	// Map keys come sorted.
	o, _ := valueOf(map[string]any{"c": 1.0, "a": 1.0, "b": 1.0}).AsObject()
	if o[0].Key != "a" || o[1].Key != "b" || o[2].Key != "c" {
		t.Errorf("map keys not sorted: %v", o)
	}
	// An unknown Go type is Unknown, never a panic.
	if v := valueOf(struct{}{}); v.Type() != driver.TypeUnknown {
		t.Errorf("valueOf(struct{}) = %s", v.Type())
	}
}

func TestToFirestore(t *testing.T) {
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	tests := []struct {
		in   driver.Value
		want any
	}{
		{driver.Null(), nil},
		{driver.Bool(false), false},
		{driver.Int(3), int64(3)},
		{driver.Float(1.25), 1.25},
		{driver.Decimal("1.50"), 1.5},
		{driver.String("x"), "x"},
		{driver.Time(ts), ts},
		{driver.JSON([]byte(`[]`)), []any{}},
		{driver.JSON([]byte(`{"a":[1,2.5,"x",null,true]}`)), map[string]any{"a": []any{int64(1), 2.5, "x", nil, true}}},
		{driver.List(driver.Int(1), driver.String("b")), []any{int64(1), "b"}},
		{driver.ObjectValue(m("k", driver.Int(1))), map[string]any{"k": int64(1)}},
		{driver.Vector([]float32{1, 2}), fs.Vector32{1, 2}},
	}
	for _, tt := range tests {
		got, err := toFirestore(tt.in)
		if err != nil {
			t.Errorf("toFirestore(%s): %v", tt.in, err)
			continue
		}
		if !equalAny(got, tt.want) {
			t.Errorf("toFirestore(%s) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
	b, err := toFirestore(driver.Bytes([]byte{9}))
	if err != nil || string(b.([]byte)) != "\x09" {
		t.Errorf("bytes: %v %v", b, err)
	}
	for _, bad := range []driver.Value{
		driver.Decimal("1e400"),
		driver.Decimal("not a number"),
		driver.JSON([]byte(`{bad`)),
		driver.JSON([]byte(`99999999999999999999`)),
		driver.ObjectValue(m("a", driver.Int(1)), m("a", driver.Int(2))),
		driver.Unknown("x", nil, "x"),
	} {
		if _, err := toFirestore(bad); err == nil {
			t.Errorf("toFirestore(%s) accepted", bad)
		}
	}
	if _, err := toFirestore(driver.Unknown("x", nil, "x")); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := toFirestore(driver.Decimal("nope")); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad decimal: %v", err)
	}
}

// parseLiteral reads a query value: plain JSON, plus tagged forms for what
// JSON lacks.
func TestParseLiteral(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		in   string
		want any
	}{
		{`null`, nil},
		{`true`, true},
		{`42`, int64(42)},
		{`-1`, int64(-1)},
		{`2.5`, 2.5},
		{`1e3`, 1000.0},
		{`"s"`, "s"},
		{`[1,"a"]`, []any{int64(1), "a"}},
		{`{"a":{"b":1}}`, map[string]any{"a": map[string]any{"b": int64(1)}}},
		{`{"$timestamp":"2026-01-02T03:04:05Z"}`, ts},
		{`{"$bytes":"AQI="}`, []byte{1, 2}},
		{`{"$geopoint":[1.5,-2]}`, &latlng.LatLng{Latitude: 1.5, Longitude: -2}},
		{`{"$vector":[1,2.5]}`, fs.Vector64{1, 2.5}},
		{`{"$double":"NaN"}`, math.NaN()},
		{`{"$double":3}`, 3.0},
		{`{"$ref":"users/ada"}`, docPath("users/ada")},
		{`[{"$ref":"a/b/c/d"}]`, []any{docPath("a/b/c/d")}},
	}
	for _, tt := range tests {
		got, err := parseLiteral([]byte(tt.in))
		if err != nil {
			t.Errorf("parseLiteral(%s): %v", tt.in, err)
			continue
		}
		if !equalAny(got, tt.want) {
			t.Errorf("parseLiteral(%s) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{
		`{"$timestamp":"yesterday"}`,
		`{"$bytes":"!!"}`,
		`{"$geopoint":[1]}`,
		`{"$geopoint":[100,0]}`,
		`{"$vector":["a"]}`,
		`{"$ref":"onlycollection"}`,
		`{"$ref":"a//b"}`,
		`{"$ref":"/a/b"}`,
		`{"$unknown":1}`,
		`{"$timestamp":"2026-01-02T03:04:05Z","extra":1}`,
		`{"a":1,"a":2}`,
		`99999999999999999999`,
		`{"$double":"many"}`,
		`[1,`,
	} {
		_, err := parseLiteral([]byte(bad))
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseLiteral(%s): want ErrSyntax, got %v", bad, err)
		}
		if err != nil && strings.Contains(err.Error(), "panic") {
			t.Errorf("parseLiteral(%s) panicked: %v", bad, err)
		}
	}
}

// equalAny compares Firestore Go values, NaN equal to NaN.
func equalAny(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || math.IsNaN(x) && math.IsNaN(y))
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equalAny(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if !equalAny(v, y[k]) {
				return false
			}
		}
		return true
	case []byte:
		y, ok := b.([]byte)
		return ok && string(x) == string(y)
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y)
	case *latlng.LatLng:
		y, ok := b.(*latlng.LatLng)
		return ok && x.Latitude == y.Latitude && x.Longitude == y.Longitude
	case fs.Vector64:
		y, ok := b.(fs.Vector64)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case fs.Vector32:
		y, ok := b.(fs.Vector32)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	return a == b
}
