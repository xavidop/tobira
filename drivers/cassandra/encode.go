package cassandra

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	"gopkg.in/inf.v0"

	"github.com/xavidop/tobira/driver"
)

// param is a bound value: it encodes a driver.Value for the column type
// the server reports for its placeholder, so a query's parameters,
// Browse conditions and Mutator values need no type hints. A value that
// does not fit the type is ErrSyntax.
type param struct {
	v driver.Value
}

func (p param) MarshalCQL(info gocql.TypeInfo) ([]byte, error) {
	b, err := encode(info, p.v, 0)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "bind", err)
	}
	return b, nil
}

// params wraps values as bound parameters.
func params(vals []driver.Value) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = param{v}
	}
	return out
}

func encode(info gocql.TypeInfo, v driver.Value, depth int) ([]byte, error) {
	if v.IsNull() {
		return nil, nil
	}
	if depth > maxDepth {
		return nil, errors.New("the value is nested too deep")
	}
	cannot := func() ([]byte, error) {
		return nil, fmt.Errorf("cannot write a %s value to a %s column", v.Type(), nativeName(info))
	}
	if raw, ok := v.AsJSON(); ok && isComposite(info) {
		jv, err := jsonValue(raw)
		if err != nil {
			return nil, err
		}
		v = jv
	}
	switch it := info.(type) {
	case gocql.CollectionType:
		return encodeCollection(it, v, depth)
	case gocql.TupleTypeInfo:
		list, ok := v.AsList()
		if !ok {
			return cannot()
		}
		if len(list) != len(it.Elems) {
			return nil, fmt.Errorf("%s needs %d elements, got %d", nativeName(info), len(it.Elems), len(list))
		}
		var buf []byte
		for i, e := range it.Elems {
			b, err := encode(e, list[i], depth+1)
			if err != nil {
				return nil, err
			}
			buf = appendField(buf, b)
		}
		return buf, nil
	case gocql.UDTTypeInfo:
		obj, ok := v.AsObject()
		if !ok {
			return cannot()
		}
		for _, m := range obj {
			if !hasUDTField(it, m.Key) {
				return nil, fmt.Errorf("the type %s has no field %q", it.Name, m.Key)
			}
		}
		var buf []byte
		for _, e := range it.Elements {
			val, _ := obj.Get(e.Name)
			b, err := encode(e.Type, val, depth+1)
			if err != nil {
				return nil, err
			}
			buf = appendField(buf, b)
		}
		return buf, nil
	case gocql.VectorType:
		return encodeVector(it, v)
	}
	switch info.Type() {
	case gocql.TypeTinyInt, gocql.TypeSmallInt, gocql.TypeInt, gocql.TypeBigInt, gocql.TypeCounter:
		n, err := toInt64(v)
		if err != nil {
			return nil, err
		}
		return gocql.Marshal(info, n)
	case gocql.TypeVarint:
		b, err := toBigInt(v)
		if err != nil {
			return nil, err
		}
		return gocql.Marshal(info, b)
	case gocql.TypeFloat, gocql.TypeDouble:
		f, err := toFloat(v)
		if err != nil {
			return nil, err
		}
		if info.Type() == gocql.TypeFloat {
			return gocql.Marshal(info, float32(f))
		}
		return gocql.Marshal(info, f)
	case gocql.TypeDecimal:
		d, err := toDec(v)
		if err != nil {
			return nil, err
		}
		return gocql.Marshal(info, d)
	case gocql.TypeBoolean:
		if b, ok := v.AsBool(); ok {
			return gocql.Marshal(info, b)
		}
		if n, ok := v.AsInt(); ok && (n == 0 || n == 1) {
			return gocql.Marshal(info, n == 1)
		}
		return cannot()
	case gocql.TypeAscii, gocql.TypeVarchar, gocql.TypeText:
		if raw, ok := v.AsJSON(); ok {
			return raw, nil
		}
		switch v.Type() {
		case driver.TypeString, driver.TypeDecimal, driver.TypeInt, driver.TypeFloat, driver.TypeBool:
			return []byte(v.String()), nil
		}
		return cannot()
	case gocql.TypeBlob:
		if b, ok := v.AsBytes(); ok {
			return b, nil
		}
		if s, ok := v.AsText(); ok && v.Type() == driver.TypeString {
			return []byte(s), nil
		}
		return cannot()
	case gocql.TypeTimestamp, gocql.TypeDate:
		t, err := toTime(v, info.Type() == gocql.TypeDate)
		if err != nil {
			return nil, err
		}
		return gocql.Marshal(info, t)
	case gocql.TypeTime:
		d, err := toTimeOfDay(v)
		if err != nil {
			return nil, err
		}
		return gocql.Marshal(info, d)
	case gocql.TypeUUID, gocql.TypeTimeUUID:
		var (
			u   gocql.UUID
			err error
		)
		if b, ok := v.AsBytes(); ok {
			u, err = gocql.UUIDFromBytes(b)
		} else if s, ok := v.AsText(); ok && v.Type() == driver.TypeString {
			u, err = gocql.ParseUUID(s)
		} else {
			return cannot()
		}
		if err != nil {
			return nil, err
		}
		return gocql.Marshal(info, u)
	case gocql.TypeInet:
		s, ok := v.AsText()
		ip := net.ParseIP(s)
		if !ok || v.Type() != driver.TypeString || ip == nil {
			return nil, fmt.Errorf("invalid inet address %s", v)
		}
		return gocql.Marshal(info, ip)
	case gocql.TypeDuration:
		s, ok := v.AsText()
		if !ok || v.Type() != driver.TypeString {
			return cannot()
		}
		d, err := parseDuration(s)
		if err != nil {
			return nil, err
		}
		return gocql.Marshal(info, d)
	}
	return cannot()
}

// isComposite reports whether a column takes a collection, tuple, UDT or
// vector, which a JSON value fills.
func isComposite(info gocql.TypeInfo) bool {
	switch info.(type) {
	case gocql.CollectionType, gocql.TupleTypeInfo, gocql.UDTTypeInfo, gocql.VectorType:
		return true
	}
	return false
}

func hasUDTField(u gocql.UDTTypeInfo, name string) bool {
	for _, e := range u.Elements {
		if e.Name == name {
			return true
		}
	}
	return false
}

// appendField appends one [int32 length][bytes] field; nil is NULL (-1).
func appendField(buf, b []byte) []byte {
	n := int32(len(b))
	if b == nil {
		n = -1
	}
	buf = binary.BigEndian.AppendUint32(buf, uint32(n))
	return append(buf, b...)
}

func encodeCollection(c gocql.CollectionType, v driver.Value, depth int) ([]byte, error) {
	if c.Type() == gocql.TypeMap {
		obj, ok := v.AsObject()
		if !ok {
			return nil, fmt.Errorf("cannot write a %s value to a %s column", v.Type(), nativeName(c))
		}
		buf := binary.BigEndian.AppendUint32(nil, uint32(len(obj)))
		for _, m := range obj {
			k, err := encode(c.Key, driver.String(m.Key), depth+1)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", m.Key, err)
			}
			if m.Value.IsNull() {
				return nil, fmt.Errorf("the value of key %q is NULL: a map holds no NULLs", m.Key)
			}
			val, err := encode(c.Elem, m.Value, depth+1)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", m.Key, err)
			}
			buf = appendField(appendField(buf, k), val)
		}
		return buf, nil
	}
	list, ok := v.AsList()
	if !ok {
		return nil, fmt.Errorf("cannot write a %s value to a %s column", v.Type(), nativeName(c))
	}
	buf := binary.BigEndian.AppendUint32(nil, uint32(len(list)))
	for i, e := range list {
		if e.IsNull() {
			return nil, fmt.Errorf("element %d is NULL: a %s holds no NULLs", i, nativeName(c))
		}
		b, err := encode(c.Elem, e, depth+1)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		buf = appendField(buf, b)
	}
	return buf, nil
}

func encodeVector(vt gocql.VectorType, v driver.Value) ([]byte, error) {
	if vec, ok := v.AsVector(); ok {
		if vt.SubType.Type() != gocql.TypeFloat {
			v = driver.List(floatValues(vec)...)
		} else {
			if len(vec) != vt.Dimensions {
				return nil, fmt.Errorf("%s needs %d dimensions, got %d", nativeName(vt), vt.Dimensions, len(vec))
			}
			return gocql.Marshal(vt, vec)
		}
	}
	list, ok := v.AsList()
	if !ok {
		return nil, fmt.Errorf("cannot write a %s value to a %s column", v.Type(), nativeName(vt))
	}
	if len(list) != vt.Dimensions {
		return nil, fmt.Errorf("%s needs %d dimensions, got %d", nativeName(vt), vt.Dimensions, len(list))
	}
	if vt.SubType.Type() == gocql.TypeFloat {
		vec := make([]float32, len(list))
		for i, e := range list {
			f, err := toFloat(e)
			if err != nil {
				return nil, err
			}
			vec[i] = float32(f)
		}
		return gocql.Marshal(vt, vec)
	}
	elems := make([]any, len(list))
	for i, e := range list {
		elems[i] = param{e}
	}
	return gocql.Marshal(vt, elems)
}

func floatValues(vec []float32) []driver.Value {
	out := make([]driver.Value, len(vec))
	for i, f := range vec {
		out[i] = driver.Float(float64(f))
	}
	return out
}

// jsonValue decodes JSON text into a driver value: objects to Object,
// arrays to List, integers to Int, other numbers to Decimal.
func jsonValue(raw []byte) (driver.Value, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return driver.Value{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return driver.Value{}, errors.New("invalid JSON: more than one value")
	}
	return fromJSON(x), nil
}

func fromJSON(x any) driver.Value {
	switch v := x.(type) {
	case nil:
		return driver.Null()
	case bool:
		return driver.Bool(v)
	case string:
		return driver.String(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return driver.Int(n)
		}
		return driver.Decimal(v.String())
	case []any:
		out := make([]driver.Value, len(v))
		for i, e := range v {
			out[i] = fromJSON(e)
		}
		return driver.List(out...)
	case map[string]any:
		out := make(driver.Object, 0, len(v))
		for k, e := range v {
			out = append(out, driver.Member{Key: k, Value: fromJSON(e)})
		}
		return driver.ObjectValue(out...)
	}
	return driver.String(fmt.Sprint(x))
}

func toInt64(v driver.Value) (int64, error) {
	if n, ok := v.AsInt(); ok {
		return n, nil
	}
	if s, ok := v.AsText(); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return n, nil
		}
	}
	if f, ok := v.AsFloat(); ok && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return int64(f), nil
	}
	return 0, fmt.Errorf("%s is not an integer", v)
}

func toBigInt(v driver.Value) (*big.Int, error) {
	if n, ok := v.AsInt(); ok {
		return big.NewInt(n), nil
	}
	if s, ok := v.AsText(); ok {
		if b, ok := new(big.Int).SetString(strings.TrimSpace(s), 10); ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%s is not an integer", v)
}

func toFloat(v driver.Value) (float64, error) {
	if f, ok := v.AsFloat(); ok {
		return f, nil
	}
	if s, ok := v.AsText(); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, nil
		}
	}
	return 0, fmt.Errorf("%s is not a number", v)
}

func toDec(v driver.Value) (*inf.Dec, error) {
	var s string
	switch v.Type() {
	case driver.TypeInt, driver.TypeDecimal, driver.TypeString:
		s = v.String()
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%s is not a decimal", v)
		}
		s = strconv.FormatFloat(f, 'f', -1, 64)
	default:
		return nil, fmt.Errorf("%s is not a decimal", v)
	}
	d, ok := new(inf.Dec).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, fmt.Errorf("%s is not a decimal", v)
	}
	return d, nil
}

// timeLayouts are the text forms a timestamp or date accepts.
var timeLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05Z07:00", "2006-01-02"}

func toTime(v driver.Value, date bool) (time.Time, error) {
	if t, ok := v.AsTime(); ok {
		if date {
			y, m, d := t.Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
		}
		return t, nil
	}
	if n, ok := v.AsInt(); ok && !date {
		return time.UnixMilli(n).UTC(), nil
	}
	if s, ok := v.AsText(); ok && v.Type() == driver.TypeString {
		for _, l := range timeLayouts {
			if t, err := time.Parse(l, strings.TrimSpace(s)); err == nil {
				return t, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("%s is not a time", v)
}

func toTimeOfDay(v driver.Value) (time.Duration, error) {
	if n, ok := v.AsInt(); ok && n >= 0 && n < int64(24*time.Hour) {
		return time.Duration(n), nil
	}
	if s, ok := v.AsText(); ok && v.Type() == driver.TypeString {
		if t, err := time.Parse("15:04:05.999999999", strings.TrimSpace(s)); err == nil {
			return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute +
				time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond()), nil
		}
	}
	return 0, fmt.Errorf("%s is not a time of day (15:04:05.999)", v)
}
