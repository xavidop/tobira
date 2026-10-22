package milvus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"

	"github.com/xavidop/tobira/driver"
)

// cellValue converts row i of a result column.
func cellValue(col column.Column, i int) driver.Value {
	if col.Nullable() {
		if isNull, err := col.IsNull(i); err != nil || isNull {
			return driver.Null()
		}
	}
	raw, err := col.Get(i)
	if err != nil {
		return driver.Null()
	}
	return anyValue(col.Type(), raw)
}

// anyValue converts one value the SDK decoded for a field of type t.
func anyValue(t entity.FieldType, raw any) driver.Value {
	switch v := raw.(type) {
	case nil:
		return driver.Null()
	case bool:
		return driver.Bool(v)
	case int8:
		return driver.Int(int64(v))
	case int16:
		return driver.Int(int64(v))
	case int32:
		return driver.Int(int64(v))
	case int64:
		return driver.Int(v)
	case float32:
		// Keep the float32's shortest spelling: 0.1 stays 0.1.
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(v), 'g', -1, 32), 64)
		return driver.Float(f)
	case float64:
		return driver.Float(v)
	case string:
		if t == entity.FieldTypeTimestamptz {
			if ts, err := time.Parse(time.RFC3339Nano, v); err == nil {
				return driver.Time(ts.UTC())
			}
		}
		return driver.String(v)
	case []byte:
		switch t {
		case entity.FieldTypeJSON:
			return driver.JSON(v)
		case entity.FieldTypeGeometry:
			return driver.String(string(v))
		}
		return driver.Bytes(v)
	case entity.FloatVector:
		return driver.Vector([]float32(v))
	case entity.Float16Vector:
		return driver.Vector([]float32(v.ToFloat32Vector()))
	case entity.BFloat16Vector:
		return driver.Vector([]float32(v.ToFloat32Vector()))
	case entity.BinaryVector:
		return driver.Bytes([]byte(v))
	case entity.Int8Vector:
		out := make([]driver.Value, len(v))
		for i, n := range v {
			out[i] = driver.Int(int64(n))
		}
		return driver.List(out...)
	case entity.SparseEmbedding:
		n := v.Len()
		idx := make([]driver.Value, 0, n)
		vals := make([]float32, 0, n)
		for i := range n {
			p, f, ok := v.Get(i)
			if !ok {
				continue
			}
			idx = append(idx, driver.Int(int64(p)))
			vals = append(vals, f)
		}
		return driver.ObjectValue(
			driver.Member{Key: "indices", Value: driver.List(idx...)},
			driver.Member{Key: "values", Value: driver.Vector(vals)},
		)
	case []bool, []int8, []int16, []int32, []int64, []float32, []float64, []string:
		return sliceValue(t, v)
	}
	// Anything newer (struct arrays, vector arrays): shown as JSON text.
	if b, err := json.Marshal(raw); err == nil {
		return driver.String(string(b))
	}
	return driver.String(fmt.Sprint(raw))
}

// sliceValue converts an Array field's elements.
func sliceValue(t entity.FieldType, raw any) driver.Value {
	var out []driver.Value
	switch v := raw.(type) {
	case []bool:
		for _, x := range v {
			out = append(out, driver.Bool(x))
		}
	case []int8:
		for _, x := range v {
			out = append(out, driver.Int(int64(x)))
		}
	case []int16:
		for _, x := range v {
			out = append(out, driver.Int(int64(x)))
		}
	case []int32:
		for _, x := range v {
			out = append(out, driver.Int(int64(x)))
		}
	case []int64:
		for _, x := range v {
			out = append(out, driver.Int(x))
		}
	case []float32:
		for _, x := range v {
			out = append(out, anyValue(t, x))
		}
	case []float64:
		for _, x := range v {
			out = append(out, driver.Float(x))
		}
	case []string:
		for _, x := range v {
			out = append(out, driver.String(x))
		}
	}
	return driver.List(out...)
}

// dynamicMembers decodes a $meta JSON object into members in key order.
// Integral numbers within int64 are Int, other numbers Float.
func dynamicMembers(raw []byte) ([]driver.Member, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("the dynamic field is not a JSON object: %w", err)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]driver.Member, len(keys))
	for i, k := range keys {
		out[i] = driver.Member{Key: k, Value: jsonValue(obj[k])}
	}
	return out, nil
}

// jsonValue converts a decoded JSON value (numbers as json.Number).
func jsonValue(x any) driver.Value {
	switch v := x.(type) {
	case nil:
		return driver.Null()
	case bool:
		return driver.Bool(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return driver.Int(i)
		}
		f, _ := v.Float64()
		return driver.Float(f)
	case string:
		return driver.String(v)
	case []any:
		out := make([]driver.Value, len(v))
		for i, it := range v {
			out[i] = jsonValue(it)
		}
		return driver.List(out...)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		ms := make([]driver.Member, len(keys))
		for i, k := range keys {
			ms[i] = driver.Member{Key: k, Value: jsonValue(v[k])}
		}
		return driver.ObjectValue(ms...)
	}
	return driver.Null()
}

// goValue converts a value to a JSON-encodable Go value (dynamic keys and
// JSON fields). Types JSON lacks are written as text: Decimal as a number
// when float64 holds it, Time as RFC 3339 in UTC, Bytes as base64.
func goValue(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case driver.TypeInt:
		i, _ := v.AsInt()
		return i, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%v is not a JSON number", f)
		}
		return f, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		return json.Number(s), nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format(time.RFC3339Nano), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		if !json.Valid(raw) {
			return nil, errors.New("invalid JSON")
		}
		return json.RawMessage(raw), nil
	case driver.TypeVector:
		vec, _ := v.AsVector()
		return vec, nil
	case driver.TypeList:
		items, _ := v.AsList()
		out := make([]any, len(items))
		for i, it := range items {
			x, err := goValue(it)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case driver.TypeObject:
		obj, _ := v.AsObject()
		out := make(map[string]any, len(obj))
		for _, m := range obj {
			x, err := goValue(m.Value)
			if err != nil {
				return nil, err
			}
			out[m.Key] = x
		}
		return out, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil // base64 in JSON
	}
	s, _ := v.AsText()
	return s, nil
}

// templateValue converts a filter value to a Milvus template value, so a
// value never becomes expression text. Lists must hold one kind of
// scalar.
func templateValue(v driver.Value) (*schemapb.TemplateValue, error) {
	unsupported := func(why string) (*schemapb.TemplateValue, error) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "filter", fmt.Errorf("cannot compare with %s %s: %s", v.Type(), v, why))
	}
	switch v.Type() {
	case driver.TypeBool:
		b, _ := v.AsBool()
		return &schemapb.TemplateValue{Val: &schemapb.TemplateValue_BoolVal{BoolVal: b}}, nil
	case driver.TypeInt:
		i, _ := v.AsInt()
		return &schemapb.TemplateValue{Val: &schemapb.TemplateValue_Int64Val{Int64Val: i}}, nil
	case driver.TypeFloat, driver.TypeDecimal:
		f, ok := v.AsFloat()
		if v.Type() == driver.TypeDecimal {
			s, _ := v.AsText()
			var err error
			f, err = strconv.ParseFloat(s, 64)
			ok = err == nil
		}
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return unsupported("not a finite number")
		}
		return &schemapb.TemplateValue{Val: &schemapb.TemplateValue_FloatVal{FloatVal: f}}, nil
	case driver.TypeString:
		s, _ := v.AsText()
		return &schemapb.TemplateValue{Val: &schemapb.TemplateValue_StringVal{StringVal: s}}, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return &schemapb.TemplateValue{Val: &schemapb.TemplateValue_StringVal{StringVal: t.UTC().Format(time.RFC3339Nano)}}, nil
	case driver.TypeList:
		items, _ := v.AsList()
		return listTemplate(items, unsupported)
	}
	return unsupported("milvus filters compare numbers, text, booleans and lists of them")
}

// listTemplate builds an array template value from homogeneous scalars.
func listTemplate(items []driver.Value, unsupported func(string) (*schemapb.TemplateValue, error)) (*schemapb.TemplateValue, error) {
	var (
		ints   []int64
		floats []float64
		strs   []string
		bools  []bool
	)
	kinds := 0
	for _, it := range items {
		tv, err := templateValue(it)
		if err != nil || tv.GetArrayVal() != nil {
			return unsupported("a list holds numbers, text or booleans")
		}
		switch x := tv.GetVal().(type) {
		case *schemapb.TemplateValue_Int64Val:
			ints = append(ints, x.Int64Val)
		case *schemapb.TemplateValue_FloatVal:
			floats = append(floats, x.FloatVal)
		case *schemapb.TemplateValue_StringVal:
			strs = append(strs, x.StringVal)
		case *schemapb.TemplateValue_BoolVal:
			bools = append(bools, x.BoolVal)
		}
	}
	for _, n := range []int{len(ints), len(floats), len(strs), len(bools)} {
		if n > 0 {
			kinds++
		}
	}
	if kinds > 1 {
		return unsupported("a list must hold one kind of value")
	}
	arr := &schemapb.TemplateArrayValue{}
	switch {
	case len(floats) > 0:
		arr.Data = &schemapb.TemplateArrayValue_DoubleData{DoubleData: &schemapb.DoubleArray{Data: floats}}
	case len(strs) > 0:
		arr.Data = &schemapb.TemplateArrayValue_StringData{StringData: &schemapb.StringArray{Data: strs}}
	case len(bools) > 0:
		arr.Data = &schemapb.TemplateArrayValue_BoolData{BoolData: &schemapb.BoolArray{Data: bools}}
	default:
		arr.Data = &schemapb.TemplateArrayValue_LongData{LongData: &schemapb.LongArray{Data: ints}}
	}
	return &schemapb.TemplateValue{Val: &schemapb.TemplateValue_ArrayVal{ArrayVal: arr}}, nil
}

// jsonTemplateValue converts a request's named parameter (JSON) to a
// template value: a string, number, boolean or a list of one of those.
func jsonTemplateValue(raw json.RawMessage) (*schemapb.TemplateValue, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, err
	}
	v := jsonValue(x)
	if v.Type() == driver.TypeNull || v.Type() == driver.TypeObject {
		return nil, errors.New("a parameter is a string, number, boolean or a list of one of those")
	}
	if items, ok := v.AsList(); ok {
		for _, it := range items {
			if it.Type() == driver.TypeList || it.Type() == driver.TypeObject || it.Type() == driver.TypeNull {
				return nil, errors.New("a list parameter holds strings, numbers or booleans")
			}
		}
	}
	tv, err := templateValue(v)
	if err != nil {
		return nil, errors.New("a list parameter must hold one kind of value")
	}
	return tv, nil
}

// columnFor builds a one-row column holding v for field f, for an insert
// or an upsert. NULL needs a nullable field (or one with a default, which
// Milvus then fills in).
func columnFor(f *fieldInfo, v driver.Value, op string) (column.Column, error) {
	bad := func(format string, args ...any) (column.Column, error) {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("field %q: "+format, append([]any{f.name}, args...)...))
	}
	if v.IsNull() {
		if !f.nullable && !f.hasDefault {
			return nil, driver.Wrap(driver.ErrConstraint, Name, op, fmt.Errorf("field %q is not nullable", f.name))
		}
		col, err := emptyColumn(f)
		if err != nil {
			return bad("%v", err)
		}
		col.SetNullable(true)
		if err := col.AppendNull(); err != nil {
			return bad("%v", err)
		}
		return col, nil
	}
	col, err := valueColumn(f, v)
	if err != nil {
		return bad("%v", err)
	}
	if f.nullable || f.hasDefault {
		col.SetNullable(true) // one validity flag per row, all valid
	}
	return col, nil
}

// emptyColumn is a zero-row column of f's type.
func emptyColumn(f *fieldInfo) (column.Column, error) {
	switch f.dataType {
	case schemapb.DataType_Bool:
		return column.NewColumnBool(f.name, nil), nil
	case schemapb.DataType_Int8:
		return column.NewColumnInt8(f.name, nil), nil
	case schemapb.DataType_Int16:
		return column.NewColumnInt16(f.name, nil), nil
	case schemapb.DataType_Int32:
		return column.NewColumnInt32(f.name, nil), nil
	case schemapb.DataType_Int64:
		return column.NewColumnInt64(f.name, nil), nil
	case schemapb.DataType_Float:
		return column.NewColumnFloat(f.name, nil), nil
	case schemapb.DataType_Double:
		return column.NewColumnDouble(f.name, nil), nil
	case schemapb.DataType_VarChar, schemapb.DataType_String:
		return column.NewColumnVarChar(f.name, nil), nil
	case schemapb.DataType_JSON:
		return column.NewColumnJSONBytes(f.name, nil), nil
	case schemapb.DataType_Timestamptz:
		return column.NewColumnTimestamptzIsoString(f.name, nil), nil
	case schemapb.DataType_Geometry:
		return column.NewColumnGeometryWKT(f.name, nil), nil
	case schemapb.DataType_FloatVector:
		return column.NewColumnFloatVector(f.name, f.dim, nil), nil
	case schemapb.DataType_Array:
		return arrayColumn(f, nil)
	}
	return nil, fmt.Errorf("type %s cannot be written by tobira: use a native client", f.nativeType())
}

// valueColumn is a one-row column holding v for f.
func valueColumn(f *fieldInfo, v driver.Value) (column.Column, error) {
	switch f.dataType {
	case schemapb.DataType_Bool:
		b, ok := v.AsBool()
		if !ok || v.Type() != driver.TypeBool {
			return nil, fmt.Errorf("needs a boolean, got %s", v.Type())
		}
		return column.NewColumnBool(f.name, []bool{b}), nil
	case schemapb.DataType_Int8, schemapb.DataType_Int16, schemapb.DataType_Int32, schemapb.DataType_Int64:
		i, err := wholeNumber(v)
		if err != nil {
			return nil, err
		}
		return intColumn(f, i)
	case schemapb.DataType_Float, schemapb.DataType_Double:
		x, err := finiteNumber(v)
		if err != nil {
			return nil, err
		}
		if f.dataType == schemapb.DataType_Float {
			return column.NewColumnFloat(f.name, []float32{float32(x)}), nil
		}
		return column.NewColumnDouble(f.name, []float64{x}), nil
	case schemapb.DataType_VarChar, schemapb.DataType_String:
		if v.Type() != driver.TypeString {
			return nil, fmt.Errorf("needs text, got %s", v.Type())
		}
		s, _ := v.AsText()
		return column.NewColumnVarChar(f.name, []string{s}), nil
	case schemapb.DataType_Geometry:
		s, ok := v.AsText()
		if !ok || v.Type() != driver.TypeString {
			return nil, fmt.Errorf("needs WKT text, got %s", v.Type())
		}
		return column.NewColumnGeometryWKT(f.name, []string{s}), nil
	case schemapb.DataType_JSON:
		raw, err := jsonBytes(v)
		if err != nil {
			return nil, err
		}
		return column.NewColumnJSONBytes(f.name, [][]byte{raw}), nil
	case schemapb.DataType_Timestamptz:
		var t time.Time
		switch v.Type() {
		case driver.TypeTime:
			t, _ = v.AsTime()
		case driver.TypeString:
			s, _ := v.AsText()
			var err error
			if t, err = time.Parse(time.RFC3339Nano, s); err != nil {
				return nil, errors.New("needs a time (RFC 3339)")
			}
		default:
			return nil, fmt.Errorf("needs a time, got %s", v.Type())
		}
		return column.NewColumnTimestamptzIsoString(f.name, []string{t.UTC().Format(time.RFC3339Nano)}), nil
	case schemapb.DataType_FloatVector:
		vec, err := floatVector(v, f.dim)
		if err != nil {
			return nil, err
		}
		return column.NewColumnFloatVector(f.name, f.dim, [][]float32{vec}), nil
	case schemapb.DataType_Array:
		items, ok := v.AsList()
		if !ok {
			return nil, fmt.Errorf("needs a list, got %s", v.Type())
		}
		return arrayColumn(f, items)
	}
	return nil, fmt.Errorf("type %s cannot be written by tobira: use a native client", f.nativeType())
}

// intColumn is a one-row integer column of f's width, refusing overflow.
func intColumn(f *fieldInfo, i int64) (column.Column, error) {
	fits := func(lo, hi int64) error {
		if i < lo || i > hi {
			return fmt.Errorf("%d does not fit %s", i, f.dataType)
		}
		return nil
	}
	switch f.dataType {
	case schemapb.DataType_Int8:
		if err := fits(math.MinInt8, math.MaxInt8); err != nil {
			return nil, err
		}
		return column.NewColumnInt8(f.name, []int8{int8(i)}), nil
	case schemapb.DataType_Int16:
		if err := fits(math.MinInt16, math.MaxInt16); err != nil {
			return nil, err
		}
		return column.NewColumnInt16(f.name, []int16{int16(i)}), nil
	case schemapb.DataType_Int32:
		if err := fits(math.MinInt32, math.MaxInt32); err != nil {
			return nil, err
		}
		return column.NewColumnInt32(f.name, []int32{int32(i)}), nil
	}
	return column.NewColumnInt64(f.name, []int64{i}), nil
}

// arrayColumn is a one-row Array column (zero rows for nil items).
func arrayColumn(f *fieldInfo, items []driver.Value) (column.Column, error) {
	rows := 1
	if items == nil {
		rows = 0
	}
	elem := &fieldInfo{name: f.name, dataType: f.elemType}
	switch f.elemType {
	case schemapb.DataType_Bool:
		vals := make([]bool, len(items))
		for i, it := range items {
			b, ok := it.AsBool()
			if !ok || it.Type() != driver.TypeBool {
				return nil, errors.New("array elements must be booleans")
			}
			vals[i] = b
		}
		return column.NewColumnBoolArray(f.name, rowsOf(vals, rows)), nil
	case schemapb.DataType_Int8, schemapb.DataType_Int16, schemapb.DataType_Int32, schemapb.DataType_Int64:
		vals := make([]int64, len(items))
		for i, it := range items {
			n, err := wholeNumber(it)
			if err != nil {
				return nil, err
			}
			if _, err := intColumn(elem, n); err != nil {
				return nil, err
			}
			vals[i] = n
		}
		switch f.elemType {
		case schemapb.DataType_Int8:
			return column.NewColumnInt8Array(f.name, rowsOf(convertInts[int8](vals), rows)), nil
		case schemapb.DataType_Int16:
			return column.NewColumnInt16Array(f.name, rowsOf(convertInts[int16](vals), rows)), nil
		case schemapb.DataType_Int32:
			return column.NewColumnInt32Array(f.name, rowsOf(convertInts[int32](vals), rows)), nil
		}
		return column.NewColumnInt64Array(f.name, rowsOf(vals, rows)), nil
	case schemapb.DataType_Float, schemapb.DataType_Double:
		vals := make([]float64, len(items))
		for i, it := range items {
			x, err := finiteNumber(it)
			if err != nil {
				return nil, err
			}
			vals[i] = x
		}
		if f.elemType == schemapb.DataType_Float {
			f32 := make([]float32, len(vals))
			for i, x := range vals {
				f32[i] = float32(x)
			}
			return column.NewColumnFloatArray(f.name, rowsOf(f32, rows)), nil
		}
		return column.NewColumnDoubleArray(f.name, rowsOf(vals, rows)), nil
	case schemapb.DataType_VarChar, schemapb.DataType_String:
		vals := make([]string, len(items))
		for i, it := range items {
			s, ok := it.AsText()
			if !ok || it.Type() != driver.TypeString {
				return nil, errors.New("array elements must be text")
			}
			vals[i] = s
		}
		return column.NewColumnVarCharArray(f.name, rowsOf(vals, rows)), nil
	}
	return nil, fmt.Errorf("arrays of %s cannot be written by tobira", f.elemType)
}

func rowsOf[T any](vals []T, rows int) [][]T {
	if rows == 0 {
		return nil
	}
	return [][]T{vals}
}

func convertInts[T int8 | int16 | int32](vals []int64) []T {
	out := make([]T, len(vals))
	for i, v := range vals {
		out[i] = T(v)
	}
	return out
}

// wholeNumber reads an integer: Int, or a Decimal, Float or digit text
// holding a whole number.
func wholeNumber(v driver.Value) (int64, error) {
	switch v.Type() {
	case driver.TypeInt:
		i, _ := v.AsInt()
		return i, nil
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		}
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if f == math.Trunc(f) && math.Abs(f) <= 1<<53 {
			return int64(f), nil
		}
	}
	return 0, fmt.Errorf("needs a whole number, got %s %s", v.Type(), v)
}

// finiteNumber reads a finite float from Int, Float or Decimal.
func finiteNumber(v driver.Value) (float64, error) {
	var (
		f  float64
		ok bool
	)
	switch v.Type() {
	case driver.TypeInt, driver.TypeFloat:
		f, ok = v.AsFloat()
	case driver.TypeDecimal:
		s, _ := v.AsText()
		x, err := strconv.ParseFloat(s, 64)
		f, ok = x, err == nil
	}
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("needs a finite number, got %s %s", v.Type(), v)
	}
	return f, nil
}

// floatVector reads a dense vector of dim finite components: a Vector or a
// List of numbers.
func floatVector(v driver.Value, dim int) ([]float32, error) {
	vec, ok := v.AsVector()
	if !ok {
		items, isList := v.AsList()
		if !isList {
			return nil, fmt.Errorf("needs a vector (a list of numbers), got %s", v.Type())
		}
		vec = make([]float32, len(items))
		for i, it := range items {
			f, err := finiteNumber(it)
			if err != nil {
				return nil, err
			}
			vec[i] = float32(f)
		}
	}
	if len(vec) != dim {
		return nil, fmt.Errorf("has %d dimensions, got %d", dim, len(vec))
	}
	for _, f := range vec {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, errors.New("has a non-finite component")
		}
	}
	return vec, nil
}

// jsonBytes encodes v for a JSON field: JSON text as is (validated), and
// any other value as its JSON form.
func jsonBytes(v driver.Value) ([]byte, error) {
	if v.Type() == driver.TypeJSON {
		raw, _ := v.AsJSON()
		if !json.Valid(raw) {
			return nil, errors.New("invalid JSON")
		}
		return raw, nil
	}
	x, err := goValue(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(x)
}
