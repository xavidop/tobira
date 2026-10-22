package firestore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/genproto/googleapis/type/latlng"

	"github.com/xavidop/tobira/driver"
)

// maxDepth caps how deep values nest when converted either way; Firestore
// itself allows 20 levels of maps and arrays.
const maxDepth = 64

// docPath is a document reference in a query literal ({"$ref": "a/b"}),
// resolved against the client when the query is built.
type docPath string

// valueOf converts a value the Firestore SDK decoded into a driver.Value:
// null, boolean, integer (Int), double (Float), string, bytes, timestamp
// (Time, UTC, microseconds), reference (its path relative to the database,
// as a String), geopoint (an Object with latitude and longitude), array
// (List), map (Object, keys sorted, as Firestore orders them) and vector
// (Vector, as float32). The MongoDB-compatible types become Int (int32),
// Decimal (decimal128) or Unknown with their display text.
func valueOf(v any) driver.Value { return valueAt(v, 0) }

func valueAt(v any, depth int) driver.Value {
	if depth > maxDepth {
		return driver.Unknown("map", nil, "(nested too deep)")
	}
	switch x := v.(type) {
	case nil:
		return driver.Null()
	case bool:
		return driver.Bool(x)
	case int64:
		return driver.Int(x)
	case float64:
		return driver.Float(x)
	case string:
		return driver.String(x)
	case []byte:
		return driver.Bytes(x)
	case time.Time:
		return driver.Time(x.UTC())
	case *fs.DocumentRef:
		if x == nil {
			return driver.Null()
		}
		return driver.String(relativePath(x.Path))
	case *latlng.LatLng:
		if x == nil {
			return driver.Null()
		}
		return driver.ObjectValue(
			driver.Member{Key: "latitude", Value: driver.Float(x.Latitude)},
			driver.Member{Key: "longitude", Value: driver.Float(x.Longitude)})
	case []any:
		out := make([]driver.Value, len(x))
		for i, e := range x {
			out[i] = valueAt(e, depth+1)
		}
		return driver.List(out...)
	case map[string]any:
		return driver.ObjectValue(objectOf(x, depth+1)...)
	case fs.Vector64:
		f := make([]float32, len(x))
		for i, e := range x {
			f[i] = float32(e)
		}
		return driver.Vector(f)
	case []float64: // DataTo decodes a vector as []float64 in some versions
		f := make([]float32, len(x))
		for i, e := range x {
			f[i] = float32(e)
		}
		return driver.Vector(f)
	case fs.BSONInt32:
		return driver.Int(int64(x))
	case fs.BSONDecimal128:
		return driver.Decimal(string(x))
	}
	text := fmt.Sprint(v)
	return driver.Unknown(nativeType(v), []byte(text), text)
}

// objectOf converts a map into an Object with its keys sorted.
func objectOf(m map[string]any, depth int) driver.Object {
	keys := slices.Sorted(maps.Keys(m))
	out := make(driver.Object, len(keys))
	for i, k := range keys {
		out[i] = driver.Member{Key: k, Value: valueAt(m[k], depth)}
	}
	return out
}

// relativePath strips "projects/p/databases/d/documents/" from a resource
// name.
func relativePath(name string) string {
	if _, rest, ok := strings.Cut(name, "/documents/"); ok {
		return rest
	}
	return name
}

// nativeType names a decoded value's Firestore type.
func nativeType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int64:
		return "integer"
	case float64:
		return "double"
	case string:
		return "string"
	case []byte:
		return "bytes"
	case time.Time:
		return "timestamp"
	case *fs.DocumentRef:
		return "reference"
	case *latlng.LatLng:
		return "geopoint"
	case []any:
		return "array"
	case map[string]any:
		return "map"
	case fs.Vector64, []float64:
		return "vector"
	case fs.BSONInt32:
		return "int32"
	case fs.BSONDecimal128:
		return "decimal128"
	case fs.BSONObjectID:
		return "objectid"
	case fs.BSONTimestamp:
		return "bsontimestamp"
	case fs.BSONRegex:
		return "regex"
	case fs.BSONBinary:
		return "bsonbinary"
	case fs.BSONMinKey:
		return "minkey"
	case fs.BSONMaxKey:
		return "maxkey"
	}
	return fmt.Sprintf("%T", v)
}

// toFirestore converts a driver.Value into a value the SDK writes:
// Decimal becomes a double (Firestore has no decimal type, so a value that
// does not fit one is ErrSyntax), JSON its parsed form (objects as maps,
// whole numbers as integers), List an array, Object a map, Vector a
// vector. Unknown is ErrUnsupported.
func toFirestore(v driver.Value) (any, error) { return toFirestoreAt(v, 0) }

func toFirestoreAt(v driver.Value, depth int) (any, error) {
	if depth > maxDepth {
		return nil, syntaxErr("value", errors.New("the value nests too deep"))
	}
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
		return f, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) {
			return nil, syntaxErr("value", fmt.Errorf("decimal %q does not fit a Firestore double", s))
		}
		return f, nil
	case driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t, nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		x, err := decodePlainJSON(raw)
		if err != nil {
			return nil, syntaxErr("value", fmt.Errorf("JSON value: %w", err))
		}
		return x, nil
	case driver.TypeVector:
		f, _ := v.AsVector()
		return fs.Vector32(f), nil
	case driver.TypeList:
		list, _ := v.AsList()
		out := make([]any, len(list))
		for i, e := range list {
			x, err := toFirestoreAt(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case driver.TypeObject:
		o, _ := v.AsObject()
		return mapOf(o, depth+1)
	}
	return nil, driver.Wrap(driver.ErrUnsupported, Name, "value", fmt.Errorf("a %s value cannot be written to Firestore", v.Type()))
}

// mapOf converts an Object into a map; a repeated key is ErrSyntax.
func mapOf(o driver.Object, depth int) (map[string]any, error) {
	out := make(map[string]any, len(o))
	for _, m := range o {
		if _, dup := out[m.Key]; dup {
			return nil, syntaxErr("value", fmt.Errorf("key %q appears twice", m.Key))
		}
		x, err := toFirestoreAt(m.Value, depth)
		if err != nil {
			return nil, err
		}
		out[m.Key] = x
	}
	return out, nil
}

func syntaxErr(op string, err error) error { return driver.Wrap(driver.ErrSyntax, Name, op, err) }

// decodePlainJSON decodes one JSON value: objects as maps (a repeated key
// is an error), whole numbers as int64, other numbers as float64.
func decodePlainJSON(raw []byte) (any, error) {
	return decodeJSON(raw, false)
}

// parseLiteral decodes a query value: plain JSON as decodePlainJSON does,
// plus one-key tagged objects for what JSON lacks:
//
//	{"$timestamp": "2026-01-02T03:04:05Z"}  RFC 3339
//	{"$bytes": "AQI="}                     standard base64
//	{"$ref": "users/ada"}                  a document reference
//	{"$geopoint": [lat, lng]}
//	{"$vector": [0.1, 0.2]}
//	{"$double": 3} or {"$double": "NaN"}   a double, also NaN and ±Infinity
//
// Any other key starting with "$" is ErrSyntax.
func parseLiteral(raw []byte) (any, error) {
	v, err := decodeJSON(raw, true)
	if err != nil {
		return nil, syntaxErr("query", err)
	}
	return v, nil
}

func decodeJSON(raw []byte, tagged bool) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := readJSON(dec, tagged, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the value")
	}
	return v, nil
}

// readJSON reads one value from dec token by token, so a repeated object
// key is seen.
func readJSON(dec *json.Decoder, tagged bool, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("the value nests too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, badJSON(err)
	}
	switch t := tok.(type) {
	case nil, bool, string:
		return t, nil
	case json.Number:
		return number(t)
	case json.Delim:
		if t == '[' {
			out := []any{}
			for dec.More() {
				e, err := readJSON(dec, tagged, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, e)
			}
			if _, err := dec.Token(); err != nil {
				return nil, badJSON(err)
			}
			return out, nil
		}
		if t == '{' {
			out := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, badJSON(err)
				}
				k := kt.(string)
				if _, dup := out[k]; dup {
					return nil, fmt.Errorf("key %q appears twice", k)
				}
				e, err := readJSON(dec, tagged, depth+1)
				if err != nil {
					return nil, err
				}
				out[k] = e
			}
			if _, err := dec.Token(); err != nil {
				return nil, badJSON(err)
			}
			if tagged {
				return untag(out)
			}
			return out, nil
		}
	}
	return nil, errors.New("not a JSON value")
}

func badJSON(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.New("unexpected end of JSON")
	}
	return fmt.Errorf("invalid JSON: %w", err)
}

// number converts a JSON number: an integer literal to int64 (out of range
// is an error), anything else to float64.
func number(n json.Number) (any, error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("integer %s does not fit 64 bits", s)
		}
		return i, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("number %s does not fit a double", s)
	}
	return f, nil
}

// untag resolves a tagged literal; an object without "$" keys is a map.
func untag(m map[string]any) (any, error) {
	tag := ""
	for k := range m {
		if strings.HasPrefix(k, "$") {
			tag = k
		}
	}
	if tag == "" {
		return m, nil
	}
	if len(m) != 1 {
		return nil, fmt.Errorf("%s must be the object's only key", tag)
	}
	v := m[tag]
	switch tag {
	case "$timestamp":
		s, _ := v.(string)
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, errors.New(`$timestamp must be an RFC 3339 string, such as "2026-01-02T03:04:05Z"`)
		}
		return t, nil
	case "$bytes":
		s, _ := v.(string)
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, errors.New("$bytes must be standard base64")
		}
		return b, nil
	case "$ref":
		s, _ := v.(string)
		if err := checkPath(s, true); err != nil {
			return nil, fmt.Errorf("$ref: %w", err)
		}
		return docPath(s), nil
	case "$geopoint":
		a, _ := v.([]any)
		if len(a) != 2 {
			return nil, errors.New("$geopoint must be [latitude, longitude]")
		}
		lat, ok1 := toFloat(a[0])
		lng, ok2 := toFloat(a[1])
		if !ok1 || !ok2 || math.Abs(lat) > 90 || math.Abs(lng) > 180 {
			return nil, errors.New("$geopoint must be [latitude, longitude] in degrees")
		}
		return &latlng.LatLng{Latitude: lat, Longitude: lng}, nil
	case "$vector":
		a, _ := v.([]any)
		out := make(fs.Vector64, len(a))
		for i, e := range a {
			f, ok := toFloat(e)
			if !ok {
				return nil, errors.New("$vector must be a list of numbers")
			}
			out[i] = f
		}
		if a == nil {
			return nil, errors.New("$vector must be a list of numbers")
		}
		return out, nil
	case "$double":
		if f, ok := toFloat(v); ok {
			return f, nil
		}
		s, _ := v.(string)
		switch s {
		case "NaN":
			return math.NaN(), nil
		case "Infinity":
			return math.Inf(1), nil
		case "-Infinity":
			return math.Inf(-1), nil
		}
		return nil, errors.New(`$double must be a number, "NaN", "Infinity" or "-Infinity"`)
	}
	return nil, fmt.Errorf("unknown tag %s (known: $timestamp, $bytes, $ref, $geopoint, $vector, $double)", tag)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// checkPath checks a slash-separated path: non-empty segments, an even
// count for a document and an odd one for a collection.
func checkPath(p string, document bool) error {
	what := "collection"
	if document {
		what = "document"
	}
	if p == "" {
		return fmt.Errorf("empty %s path", what)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "" {
			return fmt.Errorf("%s path %q has an empty segment", what, p)
		}
	}
	if document != (len(segs)%2 == 0) {
		if document {
			return fmt.Errorf("%q is not a document path: want collection/document[/collection/document…]", p)
		}
		return fmt.Errorf("%q is not a collection path: want collection[/document/collection…]", p)
	}
	return nil
}

// resolveRefs replaces docPath literals with document references.
func resolveRefs(c *fs.Client, v any) any {
	switch x := v.(type) {
	case docPath:
		return c.Doc(string(x))
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = resolveRefs(c, e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = resolveRefs(c, e)
		}
		return out
	}
	return v
}
