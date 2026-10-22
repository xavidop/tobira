package pinecone

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xavidop/tobira/driver"
)

// The columns a record row has besides its metadata keys.
const (
	idColumn       = "_id"
	valuesColumn   = "values"
	sparseColumn   = "sparse_values"
	scoreColumn    = "_score"    // Query's raw Pinecone score
	distanceColumn = "_distance" // Similar's distance, nearest first
)

// metaPrefix marks a metadata column renamed so that it cannot be taken
// for a record column: a key named like one (_id, values, sparse_values,
// _score, _distance), or one that itself starts with meta_, is shown with
// one more meta_ prefix. So "values" is column meta_values and "meta_x" is
// meta_meta_x, every key keeps its own column, and an edit of a meta_
// column always reaches the metadata key.
const metaPrefix = "meta_"

// recordColumns are the column names the driver itself produces.
var recordColumns = map[string]bool{idColumn: true, valuesColumn: true, sparseColumn: true, scoreColumn: true, distanceColumn: true}

// displayName is the column a metadata key is shown as.
func displayName(key string) string {
	if recordColumns[key] || strings.HasPrefix(key, metaPrefix) {
		return metaPrefix + key
	}
	return key
}

// metaKey is the metadata key a (non-record) column names.
func metaKey(column string) string {
	return strings.TrimPrefix(column, metaPrefix)
}

// maxExactInt is the largest integer a float64 holds exactly: a metadata
// number this size or smaller with no fraction reads as Int.
const maxExactInt = 1 << 53

// record is one Pinecone record as read.
type record struct {
	id     string
	values []float32 // nil when not read or the index is sparse
	sparse *pinecone.SparseValues
	meta   map[string]driver.Value
	score  float32
}

// newRecord converts an SDK vector (nil-safe).
func newRecord(v *pinecone.Vector) record {
	if v == nil {
		return record{}
	}
	r := record{id: v.Id, sparse: v.SparseValues, meta: metaMap(v.Metadata)}
	if v.Values != nil {
		r.values = *v.Values
	}
	return r
}

// metaMap converts record metadata.
func metaMap(s *structpb.Struct) map[string]driver.Value {
	out := make(map[string]driver.Value, len(s.GetFields()))
	for k, v := range s.GetFields() {
		out[k] = fromMeta(v)
	}
	return out
}

// fromMeta converts a metadata value. Pinecone stores every number as a
// float64: one with no fraction and within ±2^53 reads as Int, any other as
// Float. Struct members come out in key order.
func fromMeta(v *structpb.Value) driver.Value {
	switch k := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return driver.String(k.StringValue)
	case *structpb.Value_BoolValue:
		return driver.Bool(k.BoolValue)
	case *structpb.Value_NumberValue:
		f := k.NumberValue
		if f == math.Trunc(f) && math.Abs(f) <= maxExactInt {
			return driver.Int(int64(f))
		}
		return driver.Float(f)
	case *structpb.Value_ListValue:
		items := k.ListValue.GetValues()
		out := make([]driver.Value, len(items))
		for i, it := range items {
			out[i] = fromMeta(it)
		}
		return driver.List(out...)
	case *structpb.Value_StructValue:
		fields := k.StructValue.GetFields()
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		ms := make([]driver.Member, len(keys))
		for i, key := range keys {
			ms[i] = driver.Member{Key: key, Value: fromMeta(fields[key])}
		}
		return driver.ObjectValue(ms...)
	}
	return driver.Null()
}

// toMeta converts a value for metadata field: Pinecone metadata holds a
// string, a number, a boolean or a list of strings. A Time is stored as
// RFC 3339 text (UTC), a Decimal as a number when a float64 holds it
// exactly, and JSON as the string, number, boolean or list of strings it
// encodes. NULL returns nil: Pinecone has no null, so the key is left out.
func toMeta(field string, v driver.Value) (*structpb.Value, error) {
	refuse := func(format string, args ...any) (*structpb.Value, error) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "write", fmt.Errorf("metadata %q: "+format, append([]any{field}, args...)...))
	}
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeString:
		s, _ := v.AsText()
		return structpb.NewStringValue(s), nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return structpb.NewBoolValue(b), nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		if n > maxExactInt || n < -maxExactInt {
			return refuse("%d does not fit a float64 exactly, and Pinecone stores numbers as float64", n)
		}
		return structpb.NewNumberValue(float64(n)), nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return refuse("Pinecone stores finite numbers only")
		}
		return structpb.NewNumberValue(f), nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		f, err := exactFloat(s)
		if err != nil {
			return refuse("%v", err)
		}
		return structpb.NewNumberValue(f), nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return structpb.NewStringValue(t.UTC().Format(time.RFC3339Nano)), nil
	case driver.TypeList:
		items, _ := v.AsList()
		return stringList(field, items)
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return jsonMeta(field, raw)
	}
	return refuse("a %s cannot be stored: metadata holds strings, numbers, booleans and lists of strings", v.Type())
}

// exactFloat parses a decimal that a float64 holds without loss: its
// shortest float64 text must denote the same number.
func exactFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, fmt.Errorf("decimal %s is out of float64 range", s)
	}
	want, ok := new(big.Rat).SetString(s)
	got, ok2 := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if !ok || !ok2 || want.Cmp(got) != 0 {
		return 0, fmt.Errorf("decimal %s does not fit a float64 exactly, and Pinecone stores numbers as float64", s)
	}
	return f, nil
}

// stringList converts a list, which must hold strings only.
func stringList(field string, items []driver.Value) (*structpb.Value, error) {
	vals := make([]*structpb.Value, len(items))
	for i, it := range items {
		s, ok := it.AsText()
		if !ok || it.Type() != driver.TypeString {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "write", fmt.Errorf("metadata %q: a list must hold strings only, got a %s", field, it.Type()))
		}
		vals[i] = structpb.NewStringValue(s)
	}
	return structpb.NewListValue(&structpb.ListValue{Values: vals}), nil
}

// jsonMeta converts JSON text holding a string, number, boolean or list of
// strings.
func jsonMeta(field string, raw []byte) (*structpb.Value, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil || dec.More() {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "write", fmt.Errorf("metadata %q: invalid JSON", field))
	}
	switch t := x.(type) {
	case string:
		return toMeta(field, driver.String(t))
	case bool:
		return toMeta(field, driver.Bool(t))
	case json.Number:
		return toMeta(field, driver.Decimal(t.String()))
	case []any:
		items := make([]driver.Value, len(t))
		for i, it := range t {
			s, ok := it.(string)
			if !ok {
				return nil, driver.Wrap(driver.ErrUnsupported, Name, "write", fmt.Errorf("metadata %q: a JSON list must hold strings only", field))
			}
			items[i] = driver.String(s)
		}
		return stringList(field, items)
	}
	return nil, driver.Wrap(driver.ErrUnsupported, Name, "write", fmt.Errorf("metadata %q: Pinecone metadata has no objects or nulls; store a string, number, boolean or list of strings", field))
}

// shapeOptions says which columns a result has.
type shapeOptions struct {
	score   string                // scoreColumn, distanceColumn or ""
	scoreFn func(float32) float64 // turns the score into the column value
	values  bool                  // a values (and sparse_values) column
	source  *driver.CollectionRef // Column.Source, for inline edits
}

// shape fixes the columns of a result: _id, the score, the vectors, then
// the metadata keys seen in the records it was built from, sorted. A
// metadata column's type is its values' common type; Int and Float mix to
// Float, any other mix is TypeUnknown.
type shape struct {
	opts   shapeOptions
	dim    int
	sparse bool
	keys   []string
	types  map[string]driver.Type
	native map[string]string
}

func newShape(recs []record, opts shapeOptions) *shape {
	sh := &shape{opts: opts, types: map[string]driver.Type{}, native: map[string]string{}}
	seen := map[string][]driver.Value{}
	for _, r := range recs {
		if opts.values {
			if len(r.values) > 0 && sh.dim == 0 {
				sh.dim = len(r.values)
			}
			sh.sparse = sh.sparse || r.sparse != nil
		}
		for k, v := range r.meta {
			seen[k] = append(seen[k], v)
		}
	}
	for k, vals := range seen {
		sh.keys = append(sh.keys, k)
		sh.types[k], sh.native[k] = inferType(vals)
	}
	slices.SortFunc(sh.keys, func(a, b string) int { return strings.Compare(displayName(a), displayName(b)) })
	return sh
}

// warnings names the metadata keys shown under another column name.
func (sh *shape) warnings() []string {
	var out []string
	for _, k := range sh.keys {
		if d := displayName(k); d != k {
			out = append(out, fmt.Sprintf("metadata key %q is shown as %s, so it cannot be taken for a record column", k, d))
		}
	}
	return out
}

// inferType is the common type of vals and its native name.
func inferType(vals []driver.Value) (driver.Type, string) {
	var types []driver.Type
	for _, v := range vals {
		if !v.IsNull() && !slices.Contains(types, v.Type()) {
			types = append(types, v.Type())
		}
	}
	switch {
	case len(types) == 0:
		return driver.TypeNull, "null"
	case len(types) == 1:
		return types[0], nativeName(types[0])
	case len(types) == 2 && slices.Contains(types, driver.TypeInt) && slices.Contains(types, driver.TypeFloat):
		return driver.TypeFloat, "number"
	}
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = nativeName(t)
	}
	return driver.TypeUnknown, strings.Join(names, "|")
}

// nativeName is how Pinecone calls a metadata value of type t.
func nativeName(t driver.Type) string {
	switch t {
	case driver.TypeInt, driver.TypeFloat:
		return "number"
	case driver.TypeString:
		return "string"
	case driver.TypeBool:
		return "boolean"
	case driver.TypeList:
		return "list of strings"
	}
	return t.String()
}

func (sh *shape) columns() []driver.Column {
	src := sh.opts.source
	cols := []driver.Column{{Name: idColumn, NativeType: "id", Type: driver.TypeString, Source: src, Key: true}}
	if sh.opts.score != "" {
		cols = append(cols, driver.Column{Name: sh.opts.score, NativeType: "score", Type: driver.TypeFloat})
	}
	if sh.opts.values && (sh.dim > 0 || !sh.sparse) {
		native := "vector"
		if sh.dim > 0 {
			native = fmt.Sprintf("vector(%d)", sh.dim)
		}
		cols = append(cols, driver.Column{Name: valuesColumn, NativeType: native, Type: driver.TypeVector, Source: src})
	}
	if sh.opts.values && sh.sparse {
		cols = append(cols, driver.Column{Name: sparseColumn, NativeType: "sparse vector", Type: driver.TypeObject, Source: src})
	}
	for _, k := range sh.keys {
		cols = append(cols, driver.Column{Name: displayName(k), NativeType: sh.native[k], Type: sh.types[k], Source: src})
	}
	return cols
}

// row converts a record to the shape's columns. Metadata keys without a
// column are returned in dropped.
func (sh *shape) row(r record) (driver.Row, []string) {
	row := driver.Row{driver.String(r.id)}
	if sh.opts.score != "" {
		s := float64(r.score)
		if sh.opts.scoreFn != nil {
			s = sh.opts.scoreFn(r.score)
		}
		row = append(row, driver.Float(s))
	}
	if sh.opts.values && (sh.dim > 0 || !sh.sparse) {
		if r.values == nil {
			row = append(row, driver.Null())
		} else {
			row = append(row, driver.Vector(r.values))
		}
	}
	if sh.opts.values && sh.sparse {
		row = append(row, sparseValue(r.sparse))
	}
	for _, k := range sh.keys {
		v, ok := r.meta[k]
		if !ok {
			row = append(row, driver.Null())
			continue
		}
		if sh.types[k] == driver.TypeFloat && v.Type() == driver.TypeInt {
			f, _ := v.AsFloat()
			v = driver.Float(f)
		}
		row = append(row, v)
	}
	var dropped []string
	for k := range r.meta {
		if _, ok := sh.types[k]; !ok {
			dropped = append(dropped, k)
		}
	}
	slices.Sort(dropped)
	return row, dropped
}

// sparseValue shows sparse values as {indices, values}.
func sparseValue(s *pinecone.SparseValues) driver.Value {
	if s == nil {
		return driver.Null()
	}
	idx := make([]driver.Value, len(s.Indices))
	for i, n := range s.Indices {
		idx[i] = driver.Int(int64(n))
	}
	return driver.ObjectValue(
		driver.Member{Key: "indices", Value: driver.List(idx...)},
		driver.Member{Key: "values", Value: driver.Vector(s.Values)},
	)
}
