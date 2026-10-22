package driver

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestConstructAccess(t *testing.T) {
	ts := time.Date(2026, 10, 19, 1, 2, 3, 0, time.UTC)
	obj := Object{{"a", Int(1)}}
	tests := []struct {
		name string
		v    Value
		typ  Type
		got  func(Value) (any, bool)
		want any
	}{
		{"bool", Bool(true), TypeBool, func(v Value) (any, bool) { return v.AsBool() }, true},
		{"int", Int(-7), TypeInt, func(v Value) (any, bool) { return v.AsInt() }, int64(-7)},
		{"float", Float(1.5), TypeFloat, func(v Value) (any, bool) { return v.AsFloat() }, 1.5},
		{"float from int", Int(3), TypeInt, func(v Value) (any, bool) { return v.AsFloat() }, 3.0},
		{"string text", String("x"), TypeString, func(v Value) (any, bool) { return v.AsText() }, "x"},
		{"decimal text", Decimal("12.50"), TypeDecimal, func(v Value) (any, bool) { return v.AsText() }, "12.50"},
		{"bytes", Bytes([]byte{1, 2}), TypeBytes, func(v Value) (any, bool) { return v.AsBytes() }, []byte{1, 2}},
		{"time", Time(ts), TypeTime, func(v Value) (any, bool) { return v.AsTime() }, ts},
		{"json", JSON([]byte(`{"a":1}`)), TypeJSON, func(v Value) (any, bool) { return v.AsJSON() }, []byte(`{"a":1}`)},
		{"vector", Vector([]float32{1, 2}), TypeVector, func(v Value) (any, bool) { return v.AsVector() }, []float32{1, 2}},
		{"list", List(Int(1), Null()), TypeList, func(v Value) (any, bool) { return v.AsList() }, []Value{Int(1), Null()}},
		{"object", ObjectValue(obj...), TypeObject, func(v Value) (any, bool) { return v.AsObject() }, obj},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.v.Type() != tc.typ {
				t.Fatalf("Type() = %v, want %v", tc.v.Type(), tc.typ)
			}
			got, ok := tc.got(tc.v)
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got (%#v, %v), want (%#v, true)", got, ok, tc.want)
			}
		})
	}

	if !Null().IsNull() || (Value{}).Type() != TypeNull || Int(0).IsNull() {
		t.Error("IsNull wrong")
	}
	if Null().Type().String() != "null" || TypeObject.String() != "object" || Type(99).String() != "type(99)" {
		t.Error("Type.String wrong")
	}

	// Wrong-type accessors return ok=false.
	wrong := []struct {
		name string
		v    Value
		ok   func(Value) bool
	}{
		{"AsBool(int)", Int(1), func(v Value) bool { _, ok := v.AsBool(); return ok }},
		{"AsInt(float)", Float(1), func(v Value) bool { _, ok := v.AsInt(); return ok }},
		{"AsInt(decimal)", Decimal("1"), func(v Value) bool { _, ok := v.AsInt(); return ok }},
		{"AsFloat(decimal)", Decimal("1.5"), func(v Value) bool { _, ok := v.AsFloat(); return ok }},
		{"AsText(int)", Int(1), func(v Value) bool { _, ok := v.AsText(); return ok }},
		{"AsBytes(string)", String("a"), func(v Value) bool { _, ok := v.AsBytes(); return ok }},
		{"AsTime(string)", String("a"), func(v Value) bool { _, ok := v.AsTime(); return ok }},
		{"AsJSON(string)", String("{}"), func(v Value) bool { _, ok := v.AsJSON(); return ok }},
		{"AsVector(list)", List(), func(v Value) bool { _, ok := v.AsVector(); return ok }},
		{"AsList(object)", ObjectValue(), func(v Value) bool { _, ok := v.AsList(); return ok }},
		{"AsObject(list)", List(), func(v Value) bool { _, ok := v.AsObject(); return ok }},
		{"AsBool(null)", Null(), func(v Value) bool { _, ok := v.AsBool(); return ok }},
	}
	for _, tc := range wrong {
		if tc.ok(tc.v) {
			t.Errorf("%s: ok = true, want false", tc.name)
		}
	}

	if v, ok := obj.Get("a"); !ok || v.String() != "1" {
		t.Error("Object.Get hit failed")
	}
	if _, ok := obj.Get("zz"); ok {
		t.Error("Object.Get miss returned ok")
	}
}

func TestLenElementsMembers(t *testing.T) {
	list := List(Int(1), String("a"), Null())
	obj := ObjectValue(Member{"k", Int(1)}, Member{"j", Bool(true)})
	if list.Len() != 3 || obj.Len() != 2 || String("abc").Len() != 0 || Null().Len() != 0 {
		t.Errorf("Len = %d, %d, %d, %d", list.Len(), obj.Len(), String("abc").Len(), Null().Len())
	}
	var got []string
	for i, e := range list.Elements() {
		got = append(got, Int(int64(i)).String()+"="+e.String())
	}
	for i, m := range obj.Members() {
		got = append(got, Int(int64(i)).String()+"="+m.Key+":"+m.Value.String())
	}
	if want := []string{"0=1", "1=a", "2=NULL", "0=k:1", "1=j:true"}; !reflect.DeepEqual(got, want) {
		t.Errorf("iterated %q, want %q", got, want)
	}
	for range obj.Elements() {
		t.Error("an Object yields elements")
	}
	for range list.Members() {
		t.Error("a List yields members")
	}
	n := 0
	for range list.Elements() {
		n++
		break
	}
	if n != 1 {
		t.Errorf("break after %d elements", n)
	}
}

func TestValueImmutable(t *testing.T) {
	b := []byte{1, 2, 3}
	bv := Bytes(b)
	b[0] = 9
	if got, _ := bv.AsBytes(); got[0] != 1 {
		t.Error("Bytes aliases input")
	}
	got, _ := bv.AsBytes()
	got[1] = 9
	if again, _ := bv.AsBytes(); again[1] != 2 {
		t.Error("AsBytes aliases internal state")
	}

	raw := []byte(`{"a":1}`)
	jv := JSON(raw)
	raw[2] = 'z'
	if got, _ := jv.AsJSON(); string(got) != `{"a":1}` {
		t.Error("JSON aliases input")
	}

	vec := []float32{1, 2}
	vv := Vector(vec)
	vec[0] = 9
	gv, _ := vv.AsVector()
	if gv[0] != 1 {
		t.Error("Vector aliases input")
	}
	gv[1] = 9
	if again, _ := vv.AsVector(); again[1] != 2 {
		t.Error("AsVector aliases internal state")
	}

	vs := []Value{Int(1), Int(2)}
	lv := List(vs...)
	vs[0] = Int(9)
	gl, _ := lv.AsList()
	if n, _ := gl[0].AsInt(); n != 1 {
		t.Error("List aliases input")
	}
	gl[1] = Int(9)
	if again, _ := lv.AsList(); !reflect.DeepEqual(again[1], Int(2)) {
		t.Error("AsList aliases internal state")
	}

	ms := []Member{{"a", Int(1)}}
	ov := ObjectValue(ms...)
	ms[0].Key = "z"
	go1, _ := ov.AsObject()
	if go1[0].Key != "a" {
		t.Error("ObjectValue aliases input")
	}
	go1[0].Key = "q"
	if again, _ := ov.AsObject(); again[0].Key != "a" {
		t.Error("AsObject aliases internal state")
	}

	u := []byte{1}
	uv := Unknown("geometry", u, "POINT")
	u[0] = 9
	if !strings.Contains(uv.String(), "POINT") || uv.p.(unknownPayload).raw[0] != 1 {
		t.Error("Unknown aliases input")
	}
}

func TestDisplayString(t *testing.T) {
	long := make([]byte, 70)
	longWant := `\x` + strings.Repeat("00", 64) + "…"
	tests := []struct {
		name string
		v    Value
		want string
	}{
		{"null", Null(), "NULL"},
		{"bool", Bool(true), "true"},
		{"int", Int(-5), "-5"},
		{"float", Float(1.5), "1.5"},
		{"float big plain", Float(1000000), "1000000"},
		{"float small exp", Float(1.5e-7), "1.5e-07"},
		{"float zero", Float(0), "0"},
		{"float huge exp", Float(1e21), "1e+21"},
		{"float nan", Float(math.NaN()), "NaN"},
		{"float inf", Float(math.Inf(-1)), "-Inf"},
		{"decimal", Decimal("12.50"), "12.50"},
		{"string empty", String(""), ""},
		{"time whole", Time(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), "2026-01-02T03:04:05Z"},
		{"time offset", Time(time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 2*3600))), "2026-01-02T03:04:05+02:00"},
		{"time frac", Time(time.Date(2026, 1, 2, 3, 4, 5, 120000000, time.UTC)), "2026-01-02T03:04:05.12Z"},
		{"bytes short", Bytes([]byte{0xde, 0xad}), `\xdead`},
		{"bytes empty", Bytes(nil), `\x`},
		{"bytes exactly 64", Bytes(make([]byte, 64)), `\x` + strings.Repeat("00", 64)},
		{"bytes long", Bytes(long), longWant},
		{"vector", Vector([]float32{0.1, 0.2}), "[0.1, 0.2]"},
		{"vector 8", Vector([]float32{1, 2, 3, 4, 5, 6, 7, 8}), "[1, 2, 3, 4, 5, 6, 7, 8]"},
		{"vector 9", Vector([]float32{1, 2, 3, 4, 5, 6, 7, 8, 9}), "[1, 2, 3, 4, 5, 6, 7, 8, …]"},
		{"object order", ObjectValue(Member{"b", Int(1)}, Member{"a", String("x")}), `{"b":1,"a":"x"}`},
		{"list", List(Int(1), String("a"), Null()), `[1,"a",null]`},
		{"no html escape", ObjectValue(Member{"a", String("<b>&")}), `{"a":"<b>&"}`},
		{"json compacted", JSON([]byte("{ \"a\": 1 }")), `{"a":1}`},
		{"json invalid verbatim", JSON([]byte("{ nope")), "{ nope"},
		{"unknown", Unknown("geometry", []byte{1}, "POINT(1 2)"), "POINT(1 2)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		v    Value
		want string
	}{
		{"null", Null(), `null`},
		{"bool", Bool(false), `false`},
		{"int big", Int(9007199254740993), `9007199254740993`},
		{"int min", Int(math.MinInt64), `-9223372036854775808`},
		{"float", Float(1.5), `1.5`},
		{"float nan", Float(math.NaN()), `"NaN"`},
		{"float +inf", Float(math.Inf(1)), `"+Inf"`},
		{"float -inf", Float(math.Inf(-1)), `"-Inf"`},
		{"decimal verbatim", Decimal("12.50"), `12.50`},
		{"decimal huge", Decimal("18446744073709551615"), `18446744073709551615`},
		{"decimal non-number", Decimal("NaN"), `"NaN"`},
		{"string escaped", String("a\"b\n"), `"a\"b\n"`},
		{"bytes base64", Bytes([]byte("hi")), `"aGk="`},
		{"time", Time(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), `"2026-01-02T03:04:05Z"`},
		{"json raw", JSON([]byte(`{"a":[1,2]}`)), `{"a":[1,2]}`},
		{"json invalid", JSON([]byte(`{oops`)), `"{oops"`},
		{"json empty", JSON(nil), `""`},
		{"vector", Vector([]float32{0.5, float32(math.NaN())}), `[0.5,"NaN"]`},
		{"list", List(Int(1), Null(), String("x")), `[1,null,"x"]`},
		{"object order", ObjectValue(Member{"z", Int(1)}, Member{"a", List(Bool(true))}), `{"z":1,"a":[true]}`},
		{"unknown", Unknown("geometry", nil, "POINT"), `"POINT"`},
		{"no html escape", ObjectValue(Member{"a", String("<b>&")}), `{"a":"<b>&"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.v.MarshalJSON()
			if err != nil || string(got) != tc.want {
				t.Fatalf("MarshalJSON() = %s, %v; want %s", got, err, tc.want)
			}
			// Must also work through encoding/json and stay valid.
			via, err := json.Marshal(tc.v) // encoding/json re-escapes HTML itself
			if err != nil || !json.Valid(via) {
				t.Fatalf("json.Marshal = %s, %v", via, err)
			}
		})
	}
}

func TestFromAny(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		in   any
		want Value
	}{
		{"nil", nil, Null()},
		{"bool", true, Bool(true)},
		{"int", int(1), Int(1)},
		{"int8", int8(-1), Int(-1)},
		{"int16", int16(2), Int(2)},
		{"int32", int32(3), Int(3)},
		{"int64", int64(4), Int(4)},
		{"uint", uint(5), Int(5)},
		{"uint8", uint8(6), Int(6)},
		{"uint16", uint16(7), Int(7)},
		{"uint32", uint32(8), Int(8)},
		{"uint64 small", uint64(9), Int(9)},
		{"uint64 max int64", uint64(math.MaxInt64), Int(math.MaxInt64)},
		{"uint64 big", uint64(math.MaxUint64), Decimal("18446744073709551615")},
		{"uint big", uint(math.MaxUint64), Decimal("18446744073709551615")},
		{"float32", float32(1.5), Float(1.5)},
		{"float64", 2.5, Float(2.5)},
		{"string", "s", String("s")},
		{"bytes", []byte{1}, Bytes([]byte{1})},
		{"time", ts, Time(ts)},
		{"raw json", json.RawMessage(`{"a":1}`), JSON([]byte(`{"a":1}`))},
		{"value passthrough", Int(3), Int(3)},
		{"list", []any{1, "a", nil}, List(Int(1), String("a"), Null())},
		{"map sorted", map[string]any{"b": 1, "a": "x", "c": []any{true}},
			ObjectValue(Member{"a", String("x")}, Member{"b", Int(1)}, Member{"c", List(Bool(true))})},
		{"stringer", time.Second, String("1s")},
		{"nil ptr stringer", (*time.Location)(nil), Null()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromAny(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}

	errCases := map[string]any{
		"struct":       struct{ A int }{1},
		"chan":         make(chan int),
		"error":        errString("boom"),
		"nested bad":   []any{1, struct{}{}},
		"nested bad 2": map[string]any{"k": struct{}{}},
	}
	for name, in := range errCases {
		t.Run("err "+name, func(t *testing.T) {
			if _, err := FromAny(in); err == nil {
				t.Error("want error")
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestAny(t *testing.T) {
	v := ObjectValue(Member{"n", Int(1)}, Member{"l", List(Float(1.5), Null(), Decimal("2.0"))})
	want := map[string]any{"n": int64(1), "l": []any{1.5, nil, "2.0"}}
	if got := v.Any(); !reflect.DeepEqual(got, want) {
		t.Errorf("Any() = %#v, want %#v", got, want)
	}
	if Null().Any() != nil {
		t.Error("Null().Any() != nil")
	}
}

func TestAnyResultIsCopy(t *testing.T) {
	v := List(Bytes([]byte{1}), Vector([]float32{1}))
	got := v.Any().([]any)
	got[0] = "x"
	if b, ok := v.Any().([]any)[0].([]byte); !ok || b[0] != 1 {
		t.Error("Any() result aliases the Value")
	}
	bv := Bytes([]byte{1})
	bv.Any().([]byte)[0] = 9
	if b, _ := bv.AsBytes(); b[0] != 1 {
		t.Error("Any() bytes alias the Value")
	}
	jv := JSON([]byte(`1`))
	jv.Any().(json.RawMessage)[0] = '2'
	if b, _ := jv.AsJSON(); string(b) != "1" {
		t.Error("Any() json aliases the Value")
	}
}

func TestValueSize(t *testing.T) {
	if n := unsafe.Sizeof(Value{}); n > 80 {
		t.Errorf("Sizeof(Value) = %d, want <= 80", n)
	}
}

func TestAsUnknown(t *testing.T) {
	raw := []byte{1, 2}
	v := Unknown("geometry", raw, "POINT(1 2)")
	native, got, ok := v.AsUnknown()
	if !ok || native != "geometry" || !bytes.Equal(got, []byte{1, 2}) {
		t.Fatalf("AsUnknown = %q %v %v", native, got, ok)
	}
	got[0] = 9
	if _, again, _ := v.AsUnknown(); again[0] != 1 {
		t.Error("AsUnknown aliases internal state")
	}
	if n, r, ok := String("x").AsUnknown(); ok || n != "" || r != nil {
		t.Errorf("String.AsUnknown = %q %v %v, want not ok", n, r, ok)
	}
	if n, r, ok := Null().AsUnknown(); ok || n != "" || r != nil {
		t.Errorf("Null.AsUnknown = %q %v %v, want not ok", n, r, ok)
	}
}
