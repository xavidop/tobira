package mongodb

import (
	"errors"
	"math"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// rawField marshals {"v": x} and returns the value of v.
func rawField(t *testing.T, x any) bson.RawValue {
	t.Helper()
	b, err := bson.Marshal(bson.D{{Key: "v", Value: x}})
	if err != nil {
		t.Fatal(err)
	}
	return bson.Raw(b).Lookup("v")
}

func TestValueOf(t *testing.T) {
	oid, _ := bson.ObjectIDFromHex("65a1b2c3d4e5f60718293a4b")
	dec, _ := bson.ParseDecimal128("12.50")
	when := time.Date(2026, 1, 2, 3, 4, 5, 6e6, time.UTC)
	uuid := bson.Binary{Subtype: bson.TypeBinaryUUID, Data: []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}}
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	tests := []struct {
		name   string
		in     any
		want   driver.Value
		native string
	}{
		{"double", 1.5, driver.Float(1.5), "double"},
		{"nan", math.NaN(), driver.Float(math.NaN()), "double"},
		{"string", "héllo", driver.String("héllo"), "string"},
		{"int32", int32(7), driver.Int(7), "int"},
		{"int64", int64(1) << 40, driver.Int(1 << 40), "long"},
		{"bool", true, driver.Bool(true), "bool"},
		{"null", nil, driver.Null(), "null"},
		{"undefined", bson.Undefined{}, driver.Null(), "undefined"},
		{"decimal", dec, driver.Decimal("12.50"), "decimal"},
		{"date", bson.NewDateTimeFromTime(when), driver.Time(when), "date"},
		{"bytes", bson.Binary{Data: []byte{1, 2, 3}}, driver.Bytes([]byte{1, 2, 3}), "binData"},
		{"symbol", bson.Symbol("sym"), driver.String("sym"), "symbol"},
		{"object", bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: bson.D{{Key: "c", Value: "x"}}}},
			driver.ObjectValue(m("a", driver.Int(1)), m("b", driver.ObjectValue(m("c", driver.String("x"))))), "object"},
		{"array", bson.A{int32(1), "two", bson.A{}}, driver.List(driver.Int(1), driver.String("two"), driver.List()), "array"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rv := rawField(t, tc.in)
			got := valueOf(rv)
			if tc.name == "nan" {
				if f, _ := got.AsFloat(); !math.IsNaN(f) {
					t.Fatalf("got %v, want NaN", got)
				}
			} else if !drivertest.EqualValue(tc.want, got) || got.Type() != tc.want.Type() {
				t.Errorf("valueOf = %v (%s), want %v (%s)", got, got.Type(), tc.want, tc.want.Type())
			}
			if n := nativeName(rv); n != tc.native {
				t.Errorf("nativeName = %q, want %q", n, tc.native)
			}
		})
	}

	t.Run("objectId", func(t *testing.T) {
		got := valueOf(rawField(t, oid))
		native, raw, ok := got.AsUnknown()
		if !ok || native != "objectId" || len(raw) != 12 || got.String() != "65a1b2c3d4e5f60718293a4b" {
			t.Fatalf("got %v native %q raw %x", got, native, raw)
		}
	})
	t.Run("uuid", func(t *testing.T) {
		got := valueOf(rawField(t, uuid))
		native, _, ok := got.AsUnknown()
		if !ok || native != "uuid" || got.String() != "12345678-9abc-def0-1234-56789abcdef0" {
			t.Fatalf("got %v native %q", got, native)
		}
	})
	t.Run("others are unknown with a display", func(t *testing.T) {
		for name, x := range map[string]any{
			"timestamp":  bson.Timestamp{T: 5, I: 1},
			"regex":      bson.Regex{Pattern: "^a", Options: "i"},
			"javascript": bson.JavaScript("function(){}"),
			"minKey":     bson.MinKey{},
			"maxKey":     bson.MaxKey{},
			"binData":    bson.Binary{Subtype: 0x80, Data: []byte{9}},
		} {
			got := valueOf(rawField(t, x))
			native, _, ok := got.AsUnknown()
			if !ok || native != name || got.String() == "" {
				t.Errorf("%s: got %v (%s) native %q", name, got, got.Type(), native)
			}
			if _, err := got.MarshalJSON(); err != nil {
				t.Errorf("%s: MarshalJSON: %v", name, err)
			}
		}
	})
}

func TestToBSONRoundTrip(t *testing.T) {
	oid := bson.NewObjectID()
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, in := range []any{
		oid, int32(5), int64(1) << 40, 2.5, "s", true, nil,
		bson.NewDateTimeFromTime(when),
		bson.Binary{Data: []byte{1}},
		bson.Binary{Subtype: bson.TypeBinaryUUID, Data: make([]byte, 16)},
		bson.Timestamp{T: 1, I: 2},
		bson.Regex{Pattern: "a", Options: "i"},
		bson.D{{Key: "x", Value: bson.A{int32(1), "y"}}},
	} {
		rv := rawField(t, in)
		v := valueOf(rv)
		back, err := toBSON(v)
		if err != nil {
			t.Fatalf("toBSON(%v): %v", v, err)
		}
		if got := rawField(t, back); !got.Equal(rv) {
			t.Errorf("round trip of %T: got %s (%s), want %s (%s)", in, got, got.Type, rv, rv.Type)
		}
	}

	t.Run("driver values", func(t *testing.T) {
		for _, tc := range []struct {
			in   driver.Value
			want bson.Type
		}{
			{driver.Int(1 << 40), bson.TypeInt64},
			{driver.Int(3), bson.TypeInt32},
			{driver.Decimal("1.50"), bson.TypeDecimal128},
			{driver.Time(when), bson.TypeDateTime},
			{driver.JSON([]byte(`["a",{"$oid":"65a1b2c3d4e5f60718293a4b"}]`)), bson.TypeArray},
			{driver.Vector([]float32{1, 2}), bson.TypeArray},
			{driver.ObjectValue(m("a", driver.List(driver.Int(1)))), bson.TypeEmbeddedDocument},
		} {
			x, err := toBSON(tc.in)
			if err != nil {
				t.Fatalf("toBSON(%v): %v", tc.in, err)
			}
			if got := rawField(t, x); got.Type != tc.want {
				t.Errorf("toBSON(%v) type %s, want %s", tc.in, got.Type, tc.want)
			}
		}
		x, _ := toBSON(driver.JSON([]byte(`["a",{"$oid":"65a1b2c3d4e5f60718293a4b"}]`)))
		if arr := rawField(t, x).Array(); arr.Index(1).Type != bson.TypeObjectID {
			t.Errorf("JSON is read as extended JSON: got %s", arr)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for _, in := range []driver.Value{
			driver.Decimal("not-a-number"),
			driver.JSON([]byte(`{`)),
			driver.Unknown("mystery", []byte{1}, "x"),
			driver.Unknown("objectId", []byte{1, 2}, "short"),
		} {
			if _, err := toBSON(in); !errors.Is(err, driver.ErrSyntax) && !errors.Is(err, driver.ErrUnsupported) {
				t.Errorf("toBSON(%v): want ErrSyntax or ErrUnsupported, got %v", in, err)
			}
		}
	})
}
