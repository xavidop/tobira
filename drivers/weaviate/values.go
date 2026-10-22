package weaviate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/xavidop/tobira/driver"
)

// Object ids. Weaviate names every object by a UUID. tobira shows a UUID
// of the form 00000000-0000-0000-XXXX-XXXXXXXXXXXX, whose last 16 hex
// digits are at most 7fffffffffffffff, as that integer, and writes an
// integer id as such a UUID; every other UUID is a String. So a store
// seeded with integer keys keeps them, and the UUIDs sort in key order.

const intUUIDPrefix = "00000000-0000-0000-"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID reports whether s is a UUID in its canonical text form.
func isUUID(s string) bool { return uuidPattern.MatchString(s) }

// idValue is the tobira value of an object's UUID.
func idValue(uuid string) driver.Value {
	uuid = strings.ToLower(uuid)
	if rest, ok := strings.CutPrefix(uuid, intUUIDPrefix); ok && len(rest) == 17 && rest[4] == '-' {
		if n, err := strconv.ParseUint(rest[:4]+rest[5:], 16, 64); err == nil && n <= math.MaxInt64 {
			return driver.Int(int64(n))
		}
	}
	return driver.String(uuid)
}

// intUUID is the UUID that stands for the integer id n (n >= 0).
func intUUID(n int64) string {
	h := fmt.Sprintf("%016x", n)
	return intUUIDPrefix + h[:4] + "-" + h[4:]
}

var digits = regexp.MustCompile(`^[0-9]+$`)

// toUUID turns an id value into a UUID: a non-negative Int (or a String or
// Decimal of digits) becomes its integer UUID, a UUID String is kept.
func toUUID(v driver.Value, op string) (string, error) {
	bad := func() (string, error) {
		return "", driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("an object id is a UUID or a non-negative integer, got %s", v.Type()))
	}
	switch v.Type() {
	case driver.TypeInt:
		n, _ := v.AsInt()
		if n < 0 {
			return bad()
		}
		return intUUID(n), nil
	case driver.TypeString, driver.TypeDecimal:
		s, _ := v.AsText()
		if isUUID(s) {
			return strings.ToLower(s), nil
		}
		if digits.MatchString(s) {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return intUUID(n), nil
			}
		}
	}
	return bad()
}

// baseType splits a Weaviate data type into its element type and whether
// it is an array ("text[]" -> "text", true).
func baseType(dt []string) (string, bool) {
	if len(dt) != 1 {
		return "", false
	}
	t, arr := strings.CutSuffix(dt[0], "[]")
	return t, arr
}

// isReference reports whether a data type names other classes (a
// cross-reference): class names start with an upper-case letter.
func isReference(dt []string) bool {
	if len(dt) != 1 {
		return len(dt) > 1
	}
	return dt[0] != "" && dt[0][0] >= 'A' && dt[0][0] <= 'Z'
}

// typeOf is the tobira type of a Weaviate data type.
func typeOf(dt []string) driver.Type {
	if isReference(dt) {
		return driver.TypeUnknown
	}
	t, arr := baseType(dt)
	if arr {
		return driver.TypeList
	}
	switch t {
	case "text", "string", "uuid":
		return driver.TypeString
	case "int":
		return driver.TypeInt
	case "number":
		return driver.TypeFloat
	case "boolean":
		return driver.TypeBool
	case "date":
		return driver.TypeTime
	case "blob":
		return driver.TypeBytes
	case "object", "geoCoordinates", "phoneNumber":
		return driver.TypeObject
	}
	return driver.TypeUnknown
}

// nativeType spells a data type as Weaviate does ("text[]", "ref:Person").
func nativeType(dt []string) string {
	if isReference(dt) {
		return "ref:" + strings.Join(dt, "|")
	}
	return strings.Join(dt, "|")
}

// decodeProp converts a property value decoded with json.Decoder.UseNumber
// to a tobira value of its declared type. A value that does not fit the
// type is converted generically rather than lost.
func decodeProp(dt []string, nested []*models.NestedProperty, v any) driver.Value {
	if v == nil {
		return driver.Null()
	}
	if isReference(dt) {
		return generic(v)
	}
	t, arr := baseType(dt)
	if arr {
		items, ok := v.([]any)
		if !ok {
			return generic(v)
		}
		out := make([]driver.Value, len(items))
		for i, it := range items {
			out[i] = decodeProp([]string{t}, nested, it)
		}
		return driver.List(out...)
	}
	switch t {
	case "text", "string", "uuid":
		if s, ok := v.(string); ok {
			return driver.String(s)
		}
	case "int":
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				return driver.Int(i)
			}
		}
	case "number":
		if n, ok := v.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				return driver.Float(f)
			}
		}
	case "boolean":
		if b, ok := v.(bool); ok {
			return driver.Bool(b)
		}
	case "date":
		if s, ok := v.(string); ok {
			if tm, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return driver.Time(tm)
			}
		}
	case "blob":
		if s, ok := v.(string); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return driver.Bytes(b)
			}
		}
	case "object":
		if m, ok := v.(map[string]any); ok {
			return decodeObject(nested, m)
		}
	}
	return generic(v)
}

// decodeObject converts a nested object: its declared members first, in
// schema order, then any others by name. Members absent from m are left
// out (a query may select only some of them).
func decodeObject(nested []*models.NestedProperty, m map[string]any) driver.Value {
	var out []driver.Member
	seen := map[string]bool{}
	for _, np := range nested {
		if v, ok := m[np.Name]; ok {
			out = append(out, driver.Member{Key: np.Name, Value: decodeProp(np.DataType, np.NestedProperties, v)})
			seen[np.Name] = true
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, driver.Member{Key: k, Value: generic(m[k])})
	}
	return driver.ObjectValue(out...)
}

// generic converts a decoded JSON value without a declared type: integers
// are Int, other numbers Float, objects have their keys sorted.
func generic(v any) driver.Value {
	switch x := v.(type) {
	case nil:
		return driver.Null()
	case bool:
		return driver.Bool(x)
	case string:
		return driver.String(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return driver.Int(i)
		}
		if f, err := x.Float64(); err == nil {
			return driver.Float(f)
		}
		return driver.Decimal(x.String())
	case []any:
		out := make([]driver.Value, len(x))
		for i, it := range x {
			out[i] = generic(it)
		}
		return driver.List(out...)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make([]driver.Member, len(keys))
		for i, k := range keys {
			out[i] = driver.Member{Key: k, Value: generic(x[k])}
		}
		return driver.ObjectValue(out...)
	}
	return driver.String(fmt.Sprint(v))
}

// vectorValue converts a decoded vector: numbers become a Vector, an array
// of arrays (a multi-vector) a List of Vectors, an empty array NULL.
func vectorValue(v any) driver.Value {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return driver.Null()
	}
	if _, nestedArr := items[0].([]any); nestedArr {
		out := make([]driver.Value, len(items))
		for i, it := range items {
			out[i] = vectorValue(it)
		}
		return driver.List(out...)
	}
	vec := make([]float32, len(items))
	for i, it := range items {
		n, ok := it.(json.Number)
		if !ok {
			return generic(v)
		}
		f, err := n.Float64()
		if err != nil {
			return generic(v)
		}
		vec[i] = float32(f)
	}
	return driver.Vector(vec)
}

// decodeJSONText decodes raw JSON with numbers kept exact.
func decodeJSONText(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// propInfo is one property of a collection.
type propInfo struct {
	name     string
	dataType []string
	nested   []*models.NestedProperty
	desc     string
	// searchable: has a BM25 index; rangeable: has a range index.
	searchable, rangeable bool
}

// encodeProp converts a value for property p to the JSON Weaviate expects.
// NULL is null.
func encodeProp(p propInfo, v driver.Value, op string) (any, error) {
	if v.IsNull() {
		return nil, nil
	}
	if isReference(p.dataType) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("property %q is a cross-reference: edit it with a native client", p.name))
	}
	t, arr := baseType(p.dataType)
	if !arr {
		return encodeScalar(p.name, t, p.nested, v, op)
	}
	var items []driver.Value
	switch v.Type() {
	case driver.TypeList:
		items, _ = v.AsList()
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		x, err := decodeJSONText(raw)
		if err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("property %q: invalid JSON: %w", p.name, err))
		}
		if _, ok := x.([]any); !ok {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("property %q is %s: give a list", p.name, nativeType(p.dataType)))
		}
		items, _ = generic(x).AsList()
	default:
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("property %q is %s: give a list, got %s", p.name, nativeType(p.dataType), v.Type()))
	}
	out := make([]any, len(items))
	for i, it := range items {
		x, err := encodeScalar(p.name, t, p.nested, it, op)
		if err != nil {
			return nil, err
		}
		out[i] = x
	}
	return out, nil
}

var integerText = regexp.MustCompile(`^-?[0-9]+$`)

// decimalInt reads a Decimal holding an integer ("-007" is -7). Anything
// else, or a value outside int64, is ErrSyntax: GraphQL and Weaviate take
// only canonical 64-bit integers.
func decimalInt(op, field string, v driver.Value) (int64, error) {
	s, _ := v.AsText()
	if integerText.MatchString(s) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
	}
	return 0, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("property %q is an int: %q is not a 64-bit integer", field, s))
}

// errInvalidUTF8 refuses text that is not valid UTF-8: encoding it would
// replace the bad bytes with U+FFFD and match or store other text.
func errInvalidUTF8(op, field string) error {
	return driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("the text for %q is not valid UTF-8", field))
}

// encodeScalar converts one value of element type t.
func encodeScalar(name, t string, nested []*models.NestedProperty, v driver.Value, op string) (any, error) {
	bad := func() (any, error) {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("property %q is %s: cannot store a %s", name, t, v.Type()))
	}
	if v.IsNull() {
		return nil, nil
	}
	switch t {
	case "text", "string", "uuid":
		if v.Type() == driver.TypeString {
			s, _ := v.AsText()
			if !utf8.ValidString(s) {
				return nil, errInvalidUTF8(op, name)
			}
			return s, nil
		}
	case "int":
		switch v.Type() {
		case driver.TypeInt:
			n, _ := v.AsInt()
			return n, nil
		case driver.TypeFloat:
			f, _ := v.AsFloat()
			if f == math.Trunc(f) && math.Abs(f) < 1<<63 {
				return int64(f), nil
			}
		case driver.TypeDecimal:
			return decimalInt(op, name, v)
		}
	case "number":
		switch v.Type() {
		case driver.TypeInt:
			n, _ := v.AsInt()
			return n, nil
		case driver.TypeFloat:
			f, _ := v.AsFloat()
			if !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f, nil
			}
		case driver.TypeDecimal:
			s, _ := v.AsText()
			if json.Valid([]byte(s)) {
				if _, err := strconv.ParseFloat(s, 64); err == nil {
					return json.Number(s), nil
				}
			}
		}
	case "boolean":
		if b, ok := v.AsBool(); ok {
			return b, nil
		}
	case "date":
		switch v.Type() {
		case driver.TypeTime:
			tm, _ := v.AsTime()
			return tm.Format(time.RFC3339Nano), nil
		case driver.TypeString:
			s, _ := v.AsText()
			if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return s, nil
			}
		}
	case "blob":
		switch v.Type() {
		case driver.TypeBytes:
			b, _ := v.AsBytes()
			return base64.StdEncoding.EncodeToString(b), nil
		case driver.TypeString:
			s, _ := v.AsText()
			return s, nil
		}
	case "object", "geoCoordinates", "phoneNumber":
		return encodeObject(name, nested, v, op)
	}
	return bad()
}

// encodeObject converts an Object (or JSON object) for an object,
// geoCoordinates or phoneNumber property. Members of an object property
// must be declared nested properties.
func encodeObject(name string, nested []*models.NestedProperty, v driver.Value, op string) (any, error) {
	switch v.Type() {
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		x, err := decodeJSONText(raw)
		if _, ok := x.(map[string]any); err != nil || !ok {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("property %q needs a JSON object", name))
		}
		return json.RawMessage(raw), nil
	case driver.TypeObject:
		obj, _ := v.AsObject()
		out := make(map[string]any, len(obj))
		for _, m := range obj {
			if len(nested) == 0 {
				out[m.Key] = m.Value.Any()
				continue
			}
			i := slices.IndexFunc(nested, func(np *models.NestedProperty) bool { return np.Name == m.Key })
			if i < 0 {
				return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("property %q has no member %q", name, m.Key))
			}
			x, err := encodeProp(propInfo{name: name + "." + m.Key, dataType: nested[i].DataType, nested: nested[i].NestedProperties}, m.Value, op)
			if err != nil {
				return nil, err
			}
			out[m.Key] = x
		}
		return out, nil
	}
	return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("property %q needs an object, got %s", name, v.Type()))
}

// distanceOf turns Weaviate's distance into tobira's _distance, which ranks
// nearest first: l2-squared becomes the L2 distance; cosine (1 - cosine
// similarity), dot (the negated product) and the others are kept.
func distanceOf(distance string, d float64) float64 {
	if distance == "l2-squared" {
		return math.Sqrt(max(d, 0))
	}
	return d
}

// metricOf is the tobira metric of a Weaviate distance ("" for manhattan
// and hamming, which have none).
func metricOf(distance string) driver.VectorMetric {
	switch distance {
	case "l2-squared":
		return driver.MetricL2
	case "cosine":
		return driver.MetricCosine
	case "dot":
		return driver.MetricInnerProduct
	}
	return driver.MetricDefault
}
