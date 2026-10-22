package driver

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Type is the normalised type of a Value.
type Type uint8

// The Value types. TypeNull is the zero value, so the zero Value is NULL.
const (
	TypeNull    Type = iota
	TypeBool         // bool
	TypeInt          // int64
	TypeFloat        // float64
	TypeDecimal      // exact, kept as its canonical string ("12.50"); also uint64 overflow
	TypeString
	TypeBytes
	TypeTime    // time.Time; Column.NativeType says date vs timestamp vs time
	TypeJSON    // raw JSON text from the database (jsonb, JSON columns)
	TypeVector  // []float32
	TypeList    // ordered []Value
	TypeObject  // ordered []Member (keys keep order)
	TypeUnknown // native type tobira can't model: NativeType + raw bytes + display text
)

var typeNames = [...]string{
	TypeNull:    "null",
	TypeBool:    "bool",
	TypeInt:     "int",
	TypeFloat:   "float",
	TypeDecimal: "decimal",
	TypeString:  "string",
	TypeBytes:   "bytes",
	TypeTime:    "time",
	TypeJSON:    "json",
	TypeVector:  "vector",
	TypeList:    "list",
	TypeObject:  "object",
	TypeUnknown: "unknown",
}

// String returns the lowercase type name ("null", "int", ...).
func (t Type) String() string {
	if int(t) < len(typeNames) {
		return typeNames[t]
	}
	return "type(" + strconv.Itoa(int(t)) + ")"
}

// Value is an immutable tagged union carrying one database value. The zero
// Value is NULL. Constructors copy their slice arguments and accessors return
// copies, so a Value can be shared freely between goroutines.
type Value struct {
	typ Type
	n   uint64 // bool, int (as bits) or float (as bits)
	s   string // decimal, string, unknown display
	t   time.Time
	p   any // []byte (bytes, json), []float32, []Value, Object or unknownPayload
}

type unknownPayload struct {
	native string
	raw    []byte
}

// Member is one key/value pair of an Object.
type Member struct {
	Key   string
	Value Value
}

// Object is an ordered list of members; keys keep their insertion order. It is
// also the key/values type for Mutator.
type Object []Member

// Get returns the first member named key.
func (o Object) Get(key string) (Value, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return Value{}, false
}

// Null returns the NULL value.
func Null() Value { return Value{} }

// Bool returns a boolean value.
func Bool(b bool) Value {
	v := Value{typ: TypeBool}
	if b {
		v.n = 1
	}
	return v
}

// Int returns a 64-bit integer value.
func Int(i int64) Value { return Value{typ: TypeInt, n: uint64(i)} }

// Float returns a 64-bit floating point value.
func Float(f float64) Value { return Value{typ: TypeFloat, n: math.Float64bits(f)} }

// Decimal returns an exact numeric value kept as its canonical string.
func Decimal(s string) Value { return Value{typ: TypeDecimal, s: s} }

// String returns a text value.
func String(s string) Value { return Value{typ: TypeString, s: s} }

// Bytes returns a binary value; b is copied.
func Bytes(b []byte) Value { return Value{typ: TypeBytes, p: bytes.Clone(nonNil(b))} }

// Time returns a timestamp value.
func Time(t time.Time) Value { return Value{typ: TypeTime, t: t} }

// JSON returns a value holding raw JSON text; raw is copied.
func JSON(raw []byte) Value { return Value{typ: TypeJSON, p: bytes.Clone(nonNil(raw))} }

// Vector returns an embedding value; v is copied.
func Vector(v []float32) Value { return Value{typ: TypeVector, p: slices.Clone(nonNilSlice(v))} }

// List returns an ordered list value; vs is copied.
func List(vs ...Value) Value { return Value{typ: TypeList, p: slices.Clone(nonNilSlice(vs))} }

// ObjectValue returns an ordered key/value value; ms is copied.
func ObjectValue(ms ...Member) Value {
	return Value{typ: TypeObject, p: Object(slices.Clone(nonNilSlice(ms)))}
}

// Unknown returns a value of a native type tobira cannot model: the native
// type name, the raw bytes as the database sent them (copied) and the display
// text. AsUnknown returns the type name and raw bytes; String the text.
func Unknown(nativeType string, raw []byte, display string) Value {
	return Value{typ: TypeUnknown, s: display, p: unknownPayload{nativeType, bytes.Clone(nonNil(raw))}}
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Type returns the value's type.
func (v Value) Type() Type { return v.typ }

// IsNull reports whether v is NULL.
func (v Value) IsNull() bool { return v.typ == TypeNull }

// AsBool returns the boolean, or ok=false for any other type.
func (v Value) AsBool() (bool, bool) { return v.n == 1, v.typ == TypeBool }

// AsInt returns the integer, or ok=false for any other type (no lossy conversion).
func (v Value) AsInt() (int64, bool) {
	if v.typ != TypeInt {
		return 0, false
	}
	return int64(v.n), true
}

// AsFloat returns the float, also accepting Int.
func (v Value) AsFloat() (float64, bool) {
	switch v.typ {
	case TypeFloat:
		return math.Float64frombits(v.n), true
	case TypeInt:
		return float64(int64(v.n)), true
	}
	return 0, false
}

// AsText returns the text of a String or Decimal.
func (v Value) AsText() (string, bool) {
	if v.typ == TypeString || v.typ == TypeDecimal {
		return v.s, true
	}
	return "", false
}

// AsBytes returns a copy of the bytes of a Bytes value.
func (v Value) AsBytes() ([]byte, bool) {
	if v.typ != TypeBytes {
		return nil, false
	}
	return bytes.Clone(v.p.([]byte)), true
}

// AsTime returns the time of a Time value.
func (v Value) AsTime() (time.Time, bool) { return v.t, v.typ == TypeTime }

// AsJSON returns a copy of the raw JSON text of a JSON value.
func (v Value) AsJSON() ([]byte, bool) {
	if v.typ != TypeJSON {
		return nil, false
	}
	return bytes.Clone(v.p.([]byte)), true
}

// AsVector returns a copy of a Vector value.
func (v Value) AsVector() ([]float32, bool) {
	if v.typ != TypeVector {
		return nil, false
	}
	return slices.Clone(v.p.([]float32)), true
}

// AsList returns a copy of the elements of a List value.
func (v Value) AsList() ([]Value, bool) {
	if v.typ != TypeList {
		return nil, false
	}
	return slices.Clone(v.p.([]Value)), true
}

// Len is the number of elements of a List value or members of an Object
// value, and 0 for any other type.
func (v Value) Len() int {
	switch v.typ {
	case TypeList:
		return len(v.p.([]Value))
	case TypeObject:
		return len(v.p.(Object))
	}
	return 0
}

// Elements yields the elements of a List value in order without copying
// them; it yields nothing for any other type.
func (v Value) Elements() iter.Seq2[int, Value] {
	return func(yield func(int, Value) bool) {
		if v.typ != TypeList {
			return
		}
		for i, e := range v.p.([]Value) {
			if !yield(i, e) {
				return
			}
		}
	}
}

// Members yields the members of an Object value in order without copying
// them; it yields nothing for any other type.
func (v Value) Members() iter.Seq2[int, Member] {
	return func(yield func(int, Member) bool) {
		if v.typ != TypeObject {
			return
		}
		for i, m := range v.p.(Object) {
			if !yield(i, m) {
				return
			}
		}
	}
}

// AsObject returns a copy of the members of an Object value.
func (v Value) AsObject() (Object, bool) {
	if v.typ != TypeObject {
		return nil, false
	}
	return slices.Clone(v.p.(Object)), true
}

// AsUnknown returns the native type name and a copy of the raw bytes of an
// Unknown value, or ok=false for any other type.
func (v Value) AsUnknown() (nativeType string, raw []byte, ok bool) {
	if v.typ != TypeUnknown {
		return "", nil, false
	}
	u := v.p.(unknownPayload)
	return u.native, bytes.Clone(u.raw), true
}

// Any returns a plain Go value for encoding/json or templates: nil, bool,
// int64, float64, string (also for Decimal and Unknown), []byte,
// time.Time, json.RawMessage, []float32, []any and map[string]any. Object
// order is lost (a later duplicate key wins); use AsObject to keep it.
func (v Value) Any() any {
	switch v.typ {
	case TypeBool:
		return v.n == 1
	case TypeInt:
		return int64(v.n)
	case TypeFloat:
		return math.Float64frombits(v.n)
	case TypeDecimal, TypeString, TypeUnknown:
		return v.s
	case TypeBytes:
		return bytes.Clone(v.p.([]byte))
	case TypeTime:
		return v.t
	case TypeJSON:
		return json.RawMessage(bytes.Clone(v.p.([]byte)))
	case TypeVector:
		return slices.Clone(v.p.([]float32))
	case TypeList:
		l := v.p.([]Value)
		out := make([]any, len(l))
		for i, e := range l {
			out[i] = e.Any()
		}
		return out
	case TypeObject:
		o := v.p.(Object)
		out := make(map[string]any, len(o))
		for _, m := range o {
			out[m.Key] = m.Value.Any()
		}
		return out
	}
	return nil
}

const (
	maxDisplayBytes   = 64
	maxDisplayVecElem = 8
)

// String returns the display text used by the grid and table output.
func (v Value) String() string {
	switch v.typ {
	case TypeNull:
		return "NULL"
	case TypeBool:
		return strconv.FormatBool(v.n == 1)
	case TypeInt:
		return strconv.FormatInt(int64(v.n), 10)
	case TypeFloat:
		return displayFloat(math.Float64frombits(v.n))
	case TypeDecimal, TypeString, TypeUnknown:
		return v.s
	case TypeBytes:
		b, more := v.p.([]byte), ""
		if len(b) > maxDisplayBytes {
			b, more = b[:maxDisplayBytes], "…"
		}
		return `\x` + hex.EncodeToString(b) + more
	case TypeTime:
		return v.t.Format(time.RFC3339Nano)
	case TypeJSON:
		var buf bytes.Buffer
		if json.Compact(&buf, v.p.([]byte)) == nil {
			return buf.String()
		}
		return string(v.p.([]byte))
	case TypeVector:
		var sb strings.Builder
		sb.WriteByte('[')
		for i, f := range v.p.([]float32) {
			if i == maxDisplayVecElem {
				sb.WriteString(", …")
				break
			}
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(displayFloat32(f))
		}
		sb.WriteByte(']')
		return sb.String()
	case TypeList, TypeObject:
		b, _ := v.MarshalJSON()
		return string(b)
	}
	return ""
}

// displayFloat renders f without an exponent in the common range.
func displayFloat(f float64) string {
	if a := math.Abs(f); a == 0 || (a >= 1e-4 && a < 1e21) {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return formatFloat(f, 64)
}

func displayFloat32(f float32) string {
	if a := math.Abs(float64(f)); a == 0 || (a >= 1e-4 && a < 1e21) {
		return strconv.FormatFloat(float64(f), 'f', -1, 32)
	}
	return formatFloat(float64(f), 32)
}

func formatFloat(f float64, bits int) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, bits)
}

// appendJSONFloat encodes f as a JSON number, or as a string for NaN/Inf.
func appendJSONFloat(dst []byte, f float64, bits int) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.AppendQuote(dst, formatFloat(f, bits))
	}
	return strconv.AppendFloat(dst, f, 'g', -1, bits)
}

func appendJSONString(dst []byte, s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // cannot fail for a string
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}

// isJSONNumber reports whether s is a valid JSON number literal.
func isJSONNumber(s string) bool {
	if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) {
		return false
	}
	return json.Valid([]byte(s))
}

// MarshalJSON encodes v as JSON. Decimal is emitted verbatim as a number,
// Bytes as base64, NaN/Inf as the strings "NaN", "+Inf", "-Inf", JSON raw
// (as a string when it is not valid JSON) and Object with its key order.
func (v Value) MarshalJSON() ([]byte, error) {
	return v.appendJSON(nil), nil
}

func (v Value) appendJSON(dst []byte) []byte {
	switch v.typ {
	case TypeBool:
		return strconv.AppendBool(dst, v.n == 1)
	case TypeInt:
		return strconv.AppendInt(dst, int64(v.n), 10)
	case TypeFloat:
		return appendJSONFloat(dst, math.Float64frombits(v.n), 64)
	case TypeDecimal:
		if isJSONNumber(v.s) {
			return append(dst, v.s...)
		}
		return appendJSONString(dst, v.s)
	case TypeString, TypeUnknown:
		return appendJSONString(dst, v.s)
	case TypeBytes:
		return appendJSONString(dst, base64.StdEncoding.EncodeToString(v.p.([]byte)))
	case TypeTime:
		return appendJSONString(dst, v.t.Format(time.RFC3339Nano))
	case TypeJSON:
		raw := v.p.([]byte)
		if json.Valid(raw) {
			return append(dst, raw...)
		}
		return appendJSONString(dst, string(raw))
	case TypeVector:
		dst = append(dst, '[')
		for i, f := range v.p.([]float32) {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONFloat(dst, float64(f), 32)
		}
		return append(dst, ']')
	case TypeList:
		dst = append(dst, '[')
		for i, e := range v.p.([]Value) {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = e.appendJSON(dst)
		}
		return append(dst, ']')
	case TypeObject:
		dst = append(dst, '{')
		for i, m := range v.p.(Object) {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, m.Key)
			dst = append(dst, ':')
			dst = m.Value.appendJSON(dst)
		}
		return append(dst, '}')
	}
	return append(dst, "null"...)
}

// FromAny converts a plain Go value into a Value. It accepts nil, bool, all
// int and uint widths (a uint64 above MaxInt64 becomes a Decimal), float32,
// float64, string, []byte, time.Time, json.RawMessage, []any, map[string]any
// (keys sorted), Value, and any other fmt.Stringer (as a String; a nil pointer
// Stringer becomes Null). A nil []byte becomes empty Bytes, not Null: drivers
// map SQL NULL themselves. Anything
// else, including error values, is an error.
func FromAny(x any) (Value, error) {
	switch t := x.(type) {
	case nil:
		return Null(), nil
	case Value:
		return t, nil
	case bool:
		return Bool(t), nil
	case int:
		return Int(int64(t)), nil
	case int8:
		return Int(int64(t)), nil
	case int16:
		return Int(int64(t)), nil
	case int32:
		return Int(int64(t)), nil
	case int64:
		return Int(t), nil
	case uint:
		return fromUint64(uint64(t)), nil
	case uint8:
		return Int(int64(t)), nil
	case uint16:
		return Int(int64(t)), nil
	case uint32:
		return Int(int64(t)), nil
	case uint64:
		return fromUint64(t), nil
	case float32:
		return Float(float64(t)), nil
	case float64:
		return Float(t), nil
	case string:
		return String(t), nil
	case []byte:
		return Bytes(t), nil
	case time.Time:
		return Time(t), nil
	case json.RawMessage:
		return JSON(t), nil
	case []any:
		vs := make([]Value, len(t))
		for i, e := range t {
			v, err := FromAny(e)
			if err != nil {
				return Null(), fmt.Errorf("driver: list element %d: %w", i, err)
			}
			vs[i] = v
		}
		return Value{typ: TypeList, p: vs}, nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ms := make([]Member, len(keys))
		for i, k := range keys {
			v, err := FromAny(t[k])
			if err != nil {
				return Null(), fmt.Errorf("driver: object key %q: %w", k, err)
			}
			ms[i] = Member{Key: k, Value: v}
		}
		return Value{typ: TypeObject, p: Object(ms)}, nil
	case error:
		return Null(), fmt.Errorf("driver: cannot convert error value %T to a Value", x)
	case fmt.Stringer:
		if rv := reflect.ValueOf(x); rv.Kind() == reflect.Pointer && rv.IsNil() {
			return Null(), nil
		}
		return String(t.String()), nil
	}
	return Null(), fmt.Errorf("driver: cannot convert %T to a Value", x)
}

func fromUint64(u uint64) Value {
	if u > math.MaxInt64 {
		return Decimal(strconv.FormatUint(u, 10))
	}
	return Int(int64(u))
}
