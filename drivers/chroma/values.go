package chroma

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Column names the driver gives a record's parts. Metadata keys follow.
const (
	idColumn        = "id"
	documentColumn  = "document"
	embeddingColumn = "embedding"
	uriColumn       = "uri"
	distanceColumn  = "_distance"
	queryColumn     = "_query" // which query text or embedding a row answers
	// metaPrefix renames a metadata key that clashes with a column above,
	// or that itself starts with the prefix: key "id" is column "meta_id",
	// key "meta_x" is "meta_meta_x".
	metaPrefix = "meta_"
)

// reserved reports whether a metadata key would clash with a record
// column.
func reserved(key string) bool {
	switch key {
	case idColumn, documentColumn, embeddingColumn, uriColumn, distanceColumn, queryColumn:
		return true
	}
	return strings.HasPrefix(key, metaPrefix)
}

// displayName is the column for a metadata key.
func displayName(key string) string {
	if reserved(key) {
		return metaPrefix + key
	}
	return key
}

// metaKey is the metadata key for a column name (the inverse of
// displayName for metadata columns).
func metaKey(col string) string {
	if strings.HasPrefix(col, metaPrefix) {
		return col[len(metaPrefix):]
	}
	return col
}

// idValue shows a record id: Int when it is a canonical decimal int64
// ("7", "-3"; not "07", "+7" or "-0"), else String. The Mutator writes an
// Int id back as its decimal text, so the two round-trip.
func idValue(s string) driver.Value {
	if n, ok := canonicalInt(s); ok {
		return driver.Int(n)
	}
	return driver.String(s)
}

func canonicalInt(s string) (int64, bool) {
	if s == "" || s == "-0" || s[0] == '+' || (len(s) > 1 && s[0] == '0') || strings.HasPrefix(s, "-0") {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// idText converts a key value to a record id.
func idText(v driver.Value) (string, error) {
	switch v.Type() {
	case driver.TypeInt:
		n, _ := v.AsInt()
		return strconv.FormatInt(n, 10), nil
	case driver.TypeString:
		s, _ := v.AsText()
		if s != "" {
			return s, nil
		}
	}
	return "", driver.Wrap(driver.ErrSyntax, Name, "key", fmt.Errorf("a record id must be a non-empty string or an integer, got %s %s", v.Type(), v))
}

// decodeMetadata decodes one metadata object with numbers kept as
// json.Number, so integers and floats stay apart. null is a nil map.
func decodeMetadata(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("unexpected metadata from chroma: %w", err)
	}
	return m, nil
}

// metaValue converts a decoded metadata value. Chroma keeps integers and
// floats apart and spells a float with a fraction or exponent (2.0), so a
// number without one is an Int; one beyond int64 is a Float.
func metaValue(x any) driver.Value {
	switch v := x.(type) {
	case nil:
		return driver.Null()
	case bool:
		return driver.Bool(v)
	case string:
		return driver.String(v)
	case json.Number:
		s := v.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return driver.Int(n)
			}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return driver.Float(f)
	case []any:
		out := make([]driver.Value, len(v))
		for i, e := range v {
			out[i] = metaValue(e)
		}
		return driver.List(out...)
	case map[string]any:
		// A sparse vector ({"indices": [...], "values": [...]}) or any
		// other object, keys in sorted order.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		ms := make([]driver.Member, len(keys))
		for i, k := range keys {
			ms[i] = driver.Member{Key: k, Value: metaValue(v[k])}
		}
		return driver.ObjectValue(ms...)
	}
	b, _ := json.Marshal(x)
	return driver.Unknown("metadata", b, string(b))
}

// nativeType names a metadata value's type for Column.NativeType.
func nativeType(v driver.Value) string {
	switch v.Type() {
	case driver.TypeInt:
		return "int"
	case driver.TypeFloat:
		return "float"
	case driver.TypeBool:
		return "bool"
	case driver.TypeString:
		return "string"
	case driver.TypeList:
		l, _ := v.AsList()
		if len(l) > 0 {
			return nativeType(l[0]) + "[]"
		}
		return "array"
	case driver.TypeObject:
		return "object"
	}
	return v.Type().String()
}

// floatNumber spells f as a JSON number Chroma reads as a float: with a
// fraction or an exponent, so 2 is sent as 2.0.
func floatNumber(f float64) json.Number {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return json.Number(s)
}

func unsupported(op, format string, args ...any) error {
	return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf(format, args...))
}

// encodeMeta converts a value to what Chroma metadata can hold: a bool,
// int, float or string, or a non-empty array of one of them. Time becomes
// RFC 3339 text in UTC and JSON its compact text, because Chroma has no
// time or object type (and drops empty arrays). A Decimal is written as a
// float only when the float holds it exactly. NULL is nil (the caller
// leaves it out on insert, and sends it to remove the key on update).
func encodeMeta(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		return n, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, unsupported("encode", "chroma cannot store %v", f)
		}
		return floatNumber(f), nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		f, err := exactFloat(s)
		if err != nil {
			return nil, err
		}
		return floatNumber(f), nil
	case driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format(time.RFC3339Nano), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "encode", errors.New("invalid JSON value"))
		}
		return buf.String(), nil
	case driver.TypeList:
		return encodeList(v)
	}
	return nil, unsupported("encode", "chroma metadata cannot hold a %s value (only bool, int, float, string and arrays of one of them)", v.Type())
}

// encodeList encodes a non-empty array of one scalar type. Ints and floats
// mix as floats.
func encodeList(v driver.Value) (any, error) {
	l, _ := v.AsList()
	if len(l) == 0 {
		return nil, unsupported("encode", "chroma does not store empty arrays (it drops them): write NULL or a JSON value instead")
	}
	kind := ""
	for _, e := range l {
		k := ""
		switch e.Type() {
		case driver.TypeString:
			k = "string"
		case driver.TypeBool:
			k = "bool"
		case driver.TypeInt, driver.TypeFloat, driver.TypeDecimal:
			k = "number"
		default:
			return nil, unsupported("encode", "a chroma array holds only bools, numbers or strings, not %s", e.Type())
		}
		if kind != "" && k != kind {
			return nil, unsupported("encode", "a chroma array holds one type of value, not %s and %s", kind, k)
		}
		kind = k
	}
	anyFloat := false
	for _, e := range l {
		anyFloat = anyFloat || e.Type() != driver.TypeInt
	}
	out := make([]any, len(l))
	for i, e := range l {
		x, err := encodeMeta(e)
		if err != nil {
			return nil, err
		}
		if n, ok := x.(int64); ok && anyFloat && kind == "number" {
			x = floatNumber(float64(n))
		}
		out[i] = x
	}
	return out, nil
}

// exactFloat parses a decimal when the shortest float64 spelling denotes
// the same number ("37.13" yes, "0.1000000000000000000001" no).
func exactFloat(s string) (float64, error) {
	want, ok := new(big.Rat).SetString(s)
	f, err := strconv.ParseFloat(s, 64)
	if !ok || err != nil || math.IsInf(f, 0) {
		return 0, unsupported("encode", "decimal %s does not fit a float", s)
	}
	got, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if got == nil || got.Cmp(want) != 0 {
		return 0, unsupported("encode", "decimal %s would lose precision as a float, which is all chroma stores", s)
	}
	return f, nil
}

// embeddingOf reads an embedding: a Vector, a list of numbers, or JSON or
// text holding an array of numbers. Components must be finite.
func embeddingOf(v driver.Value) ([]float32, error) {
	bad := func(why string) ([]float32, error) {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "encode", fmt.Errorf("embedding: %s", why))
	}
	var out []float32
	switch v.Type() {
	case driver.TypeVector:
		out, _ = v.AsVector()
	case driver.TypeList:
		l, _ := v.AsList()
		for _, e := range l {
			f, ok := e.AsFloat()
			if !ok || (e.Type() != driver.TypeInt && e.Type() != driver.TypeFloat && e.Type() != driver.TypeDecimal) {
				return bad("want numbers")
			}
			out = append(out, float32(f))
		}
	case driver.TypeJSON, driver.TypeString:
		var raw []byte
		if v.Type() == driver.TypeJSON {
			raw, _ = v.AsJSON()
		} else {
			s, _ := v.AsText()
			raw = []byte(s)
		}
		var fs []float64
		if err := json.Unmarshal(raw, &fs); err != nil {
			return bad("want an array of numbers")
		}
		for _, f := range fs {
			out = append(out, float32(f))
		}
	default:
		return bad("want a vector or an array of numbers, got " + v.Type().String())
	}
	if len(out) == 0 {
		return bad("it is empty")
	}
	if err := finite(out); err != nil {
		return nil, err
	}
	return out, nil
}

// finite refuses NaN and infinite components.
func finite(v []float32) error {
	for _, f := range v {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return driver.Wrap(driver.ErrSyntax, Name, "encode", errors.New("embedding components must be finite numbers"))
		}
	}
	return nil
}

// numbersToVector converts an embedding from a reply.
func numbersToVector(ns []json.Number) []float32 {
	out := make([]float32, len(ns))
	for i, n := range ns {
		f, _ := n.Float64()
		out[i] = float32(f)
	}
	return out
}
