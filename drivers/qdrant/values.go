package qdrant

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

// fieldType is how a payload field is shown. Payloads are JSON, so a value
// is shown as its JSON type (typeNatural) unless the collection says
// otherwise: a datetime payload index makes it typeTime, and the
// collection metadata can declare any field's type (see schemaFromInfo).
type fieldType string

const (
	typeNatural fieldType = ""
	typeString  fieldType = "string"
	typeInt     fieldType = "int"
	typeFloat   fieldType = "float"
	typeBool    fieldType = "bool"
	typeDecimal fieldType = "decimal"
	typeTime    fieldType = "time"
	typeJSON    fieldType = "json"
)

// driverType is the driver.Type a declared field type produces.
func (t fieldType) driverType() driver.Type {
	switch t {
	case typeString:
		return driver.TypeString
	case typeInt:
		return driver.TypeInt
	case typeFloat:
		return driver.TypeFloat
	case typeBool:
		return driver.TypeBool
	case typeDecimal:
		return driver.TypeDecimal
	case typeTime:
		return driver.TypeTime
	case typeJSON:
		return driver.TypeJSON
	}
	return driver.TypeNull
}

// fromValue converts a payload value by its JSON type. Struct members come
// out in key order (proto maps have none), so rows are deterministic.
func fromValue(v *pb.Value) driver.Value {
	switch k := v.GetKind().(type) {
	case *pb.Value_BoolValue:
		return driver.Bool(k.BoolValue)
	case *pb.Value_IntegerValue:
		return driver.Int(k.IntegerValue)
	case *pb.Value_DoubleValue:
		return driver.Float(k.DoubleValue)
	case *pb.Value_StringValue:
		return driver.String(k.StringValue)
	case *pb.Value_ListValue:
		items := k.ListValue.GetValues()
		out := make([]driver.Value, len(items))
		for i, it := range items {
			out[i] = fromValue(it)
		}
		return driver.List(out...)
	case *pb.Value_StructValue:
		return driver.ObjectValue(structMembers(k.StructValue.GetFields())...)
	}
	return driver.Null()
}

func structMembers(fields map[string]*pb.Value) []driver.Member {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	ms := make([]driver.Member, len(keys))
	for i, k := range keys {
		ms[i] = driver.Member{Key: k, Value: fromValue(fields[k])}
	}
	return ms
}

// decimalText matches a plain decimal number.
var decimalText = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// timeLayouts are the datetime spellings Qdrant's datetime index accepts.
var timeLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02"}

// typedValue converts v as field type t. A value that does not fit t is
// shown by its JSON type instead, never dropped: a declaration is a display
// hint, not a constraint Qdrant enforces.
func typedValue(v *pb.Value, t fieldType) driver.Value {
	nat := fromValue(v)
	if nat.IsNull() || t == typeNatural {
		return nat
	}
	switch t {
	case typeString:
		if s, ok := nat.AsText(); ok {
			return driver.String(s)
		}
		if nat.Type() == driver.TypeObject || nat.Type() == driver.TypeList {
			return nat
		}
		return driver.String(nat.String())
	case typeInt:
		if nat.Type() == driver.TypeInt {
			return nat // exact: Qdrant integers are int64
		}
		// A double converts only when it is an integer float64 holds
		// exactly, so no value is ever rounded.
		if f, ok := nat.AsFloat(); ok && f == math.Trunc(f) && math.Abs(f) <= 1<<53 {
			return driver.Int(int64(f))
		}
	case typeFloat:
		if f, ok := nat.AsFloat(); ok {
			return driver.Float(f)
		}
	case typeBool:
		if nat.Type() == driver.TypeBool {
			return nat
		}
	case typeDecimal:
		switch nat.Type() {
		case driver.TypeInt:
			return driver.Decimal(nat.String())
		case driver.TypeFloat:
			f, _ := nat.AsFloat()
			return driver.Decimal(strconv.FormatFloat(f, 'f', -1, 64))
		case driver.TypeString:
			s, _ := nat.AsText()
			if decimalText.MatchString(s) {
				return driver.Decimal(s)
			}
		}
	case typeTime:
		if s, ok := nat.AsText(); ok {
			for _, l := range timeLayouts {
				if ts, err := time.Parse(l, s); err == nil {
					return driver.Time(ts.UTC())
				}
			}
		}
	case typeJSON:
		raw, err := json.Marshal(nat)
		if err == nil {
			return driver.JSON(raw)
		}
	}
	return nat
}

// toValue converts a driver value to a payload value. Types JSON lacks are
// written as text: Decimal as its exact digits, Time as RFC 3339 in UTC and
// Bytes as base64.
func toValue(v driver.Value) (*pb.Value, error) {
	switch v.Type() {
	case driver.TypeNull:
		return pb.NewValueNull(), nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return pb.NewValueBool(b), nil
	case driver.TypeInt:
		i, _ := v.AsInt()
		return pb.NewValueInt(i), nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "encode", fmt.Errorf("%v is not a JSON number", f))
		}
		return pb.NewValueDouble(f), nil
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		return pb.NewValueString(s), nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return pb.NewValueString(base64.StdEncoding.EncodeToString(b)), nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return pb.NewValueString(t.UTC().Format(time.RFC3339Nano)), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return jsonToValue(raw)
	case driver.TypeVector:
		vec, _ := v.AsVector()
		items := make([]*pb.Value, len(vec))
		for i, f := range vec {
			items[i] = pb.NewValueDouble(float64(f))
		}
		return pb.NewValueFromList(items...), nil
	case driver.TypeList:
		vs, _ := v.AsList()
		items := make([]*pb.Value, len(vs))
		for i, x := range vs {
			pv, err := toValue(x)
			if err != nil {
				return nil, err
			}
			items[i] = pv
		}
		return pb.NewValueFromList(items...), nil
	case driver.TypeObject:
		obj, _ := v.AsObject()
		fields := make(map[string]*pb.Value, len(obj))
		for _, m := range obj {
			pv, err := toValue(m.Value)
			if err != nil {
				return nil, err
			}
			fields[m.Key] = pv
		}
		return pb.NewValueFromFields(fields), nil
	}
	return pb.NewValueString(v.String()), nil
}

// encodeLike encodes v for a key whose stored value is cur (nil when there
// is none). A Decimal written over a stored JSON number stays a number (an
// integer when it has no fraction), so editing a decimal-declared field
// never turns a numeric key, its range filters and its index into text. A
// decimal a float64 cannot hold (overflow) is written as text.
func encodeLike(v driver.Value, cur *pb.Value) (*pb.Value, error) {
	if v.Type() == driver.TypeDecimal && cur != nil {
		switch cur.GetKind().(type) {
		case *pb.Value_IntegerValue, *pb.Value_DoubleValue:
			s, _ := v.AsText()
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return pb.NewValueInt(i), nil
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) {
				if f == math.Trunc(f) && math.Abs(f) <= 1<<53 {
					return pb.NewValueInt(int64(f)), nil
				}
				return pb.NewValueDouble(f), nil
			}
		}
	}
	return toValue(v)
}

// jsonToValue parses JSON text into a payload value, keeping integers
// exact.
func jsonToValue(raw []byte) (*pb.Value, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "encode", fmt.Errorf("invalid JSON value: %w", err))
	}
	if dec.More() {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "encode", errors.New("invalid JSON value: trailing data"))
	}
	return anyToValue(x), nil
}

func anyToValue(x any) *pb.Value {
	switch t := x.(type) {
	case bool:
		return pb.NewValueBool(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return pb.NewValueInt(i)
		}
		f, _ := t.Float64()
		return pb.NewValueDouble(f)
	case string:
		return pb.NewValueString(t)
	case []any:
		items := make([]*pb.Value, len(t))
		for i, it := range t {
			items[i] = anyToValue(it)
		}
		return pb.NewValueFromList(items...)
	case map[string]any:
		fields := make(map[string]*pb.Value, len(t))
		for k, it := range t {
			fields[k] = anyToValue(it)
		}
		return pb.NewValueFromFields(fields)
	}
	return pb.NewValueNull()
}

// idValue shows a point id: Int for a number (Decimal above MaxInt64),
// String for a UUID.
func idValue(id *pb.PointId) driver.Value {
	switch k := id.GetPointIdOptions().(type) {
	case *pb.PointId_Num:
		if k.Num > math.MaxInt64 {
			return driver.Decimal(strconv.FormatUint(k.Num, 10))
		}
		return driver.Int(int64(k.Num))
	case *pb.PointId_Uuid:
		return driver.String(k.Uuid)
	}
	return driver.Null()
}

// uuidText matches a UUID in any of the forms Qdrant accepts.
var uuidText = regexp.MustCompile(`^(?i)(urn:uuid:)?\{?[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}\}?$`)

// pointID converts a key value to a point id: an unsigned integer (Int,
// Decimal or digit text) or a UUID string.
func pointID(v driver.Value) (*pb.PointId, error) {
	bad := func() (*pb.PointId, error) {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "key", fmt.Errorf("point id must be an unsigned integer or a UUID, got %s %s", v.Type(), v))
	}
	switch v.Type() {
	case driver.TypeInt:
		i, _ := v.AsInt()
		if i < 0 {
			return bad()
		}
		return pb.NewIDNum(uint64(i)), nil
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		if n, ok := new(big.Int).SetString(s, 10); ok && n.Sign() >= 0 && n.IsUint64() {
			return pb.NewIDNum(n.Uint64()), nil
		}
		if v.Type() == driver.TypeString && uuidText.MatchString(s) {
			return pb.NewIDUUID(s), nil
		}
	}
	return bad()
}

// idString is a point id as cursor text: "num:7" or "uuid:…".
func idString(id *pb.PointId) string {
	switch k := id.GetPointIdOptions().(type) {
	case *pb.PointId_Num:
		return "num:" + strconv.FormatUint(k.Num, 10)
	case *pb.PointId_Uuid:
		return "uuid:" + k.Uuid
	}
	return ""
}

// parseIDString is the inverse of idString.
func parseIDString(s string) (*pb.PointId, bool) {
	switch {
	case strings.HasPrefix(s, "num:"):
		n, err := strconv.ParseUint(s[len("num:"):], 10, 64)
		return pb.NewIDNum(n), err == nil
	case strings.HasPrefix(s, "uuid:") && uuidText.MatchString(s[len("uuid:"):]):
		return pb.NewIDUUID(s[len("uuid:"):]), true
	}
	return nil, false
}

// vectorValue shows a stored vector: a dense one as a Vector, a sparse one
// as an Object of indices and values, a multi-vector as a List of Vectors.
func vectorValue(v *pb.VectorOutput) driver.Value {
	if v == nil {
		return driver.Null()
	}
	switch k := v.GetVector().(type) {
	case *pb.VectorOutput_Dense:
		return driver.Vector(k.Dense.GetData())
	case *pb.VectorOutput_Sparse:
		return sparseValue(k.Sparse.GetIndices(), k.Sparse.GetValues())
	case *pb.VectorOutput_MultiDense:
		vs := k.MultiDense.GetVectors()
		out := make([]driver.Value, len(vs))
		for i, d := range vs {
			out[i] = driver.Vector(d.GetData())
		}
		return driver.List(out...)
	}
	// Servers before 1.12 fill only the deprecated flat fields.
	data := v.GetData()                    //nolint:staticcheck // fallback for old servers
	if idx := v.GetIndices(); idx != nil { //nolint:staticcheck // fallback for old servers
		return sparseValue(idx.GetData(), data)
	}
	if len(data) == 0 {
		return driver.Null()
	}
	return driver.Vector(data)
}

func sparseValue(indices []uint32, values []float32) driver.Value {
	idx := make([]driver.Value, len(indices))
	for i, n := range indices {
		idx[i] = driver.Int(int64(n))
	}
	return driver.ObjectValue(
		driver.Member{Key: "indices", Value: driver.List(idx...)},
		driver.Member{Key: "values", Value: driver.Vector(values)},
	)
}
