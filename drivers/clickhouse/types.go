package clickhouse

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/xavidop/tobira/driver"
)

// unwrapType strips the wrappers that do not change what a value is:
// LowCardinality(T), Nullable(T) and SimpleAggregateFunction(f, T).
func unwrapType(native string) string {
	t := strings.TrimSpace(native)
	for {
		switch {
		case hasWrapper(t, "LowCardinality"):
			t = strings.TrimSpace(t[len("LowCardinality(") : len(t)-1])
		case hasWrapper(t, "Nullable"):
			t = strings.TrimSpace(t[len("Nullable(") : len(t)-1])
		case hasWrapper(t, "SimpleAggregateFunction"):
			inner := t[len("SimpleAggregateFunction(") : len(t)-1]
			_, typ, ok := strings.Cut(inner, ",")
			if !ok {
				return t
			}
			t = strings.TrimSpace(typ)
		default:
			return t
		}
	}
}

func hasWrapper(t, name string) bool {
	return strings.HasPrefix(t, name+"(") && strings.HasSuffix(t, ")")
}

// baseName is a type's name without its parameters: "Decimal" for
// "Decimal(10, 2)".
func baseName(t string) string {
	name, _, _ := strings.Cut(t, "(")
	return strings.TrimSpace(name)
}

// nullable reports whether a column type admits NULL.
func nullable(native string) bool {
	t := strings.TrimSpace(native)
	if hasWrapper(t, "LowCardinality") {
		t = strings.TrimSpace(t[len("LowCardinality(") : len(t)-1])
	}
	return hasWrapper(t, "Nullable")
}

// columnType normalises a ClickHouse type name. UInt64 is Decimal, since
// its values can exceed int64. Composite types (Array, Map, Tuple, Nested,
// JSON, Variant, Dynamic and the geo types) are JSON: their values always
// come back as JSON text.
func columnType(native string) driver.Type {
	switch name := baseName(unwrapType(native)); name {
	case "Int8", "Int16", "Int32", "Int64", "UInt8", "UInt16", "UInt32":
		return driver.TypeInt
	case "UInt64", "Int128", "Int256", "UInt128", "UInt256",
		"Decimal", "Decimal32", "Decimal64", "Decimal128", "Decimal256":
		return driver.TypeDecimal
	case "Float32", "Float64", "BFloat16":
		return driver.TypeFloat
	case "Bool", "Boolean":
		return driver.TypeBool
	case "String", "FixedString", "UUID", "IPv4", "IPv6", "Enum", "Enum8", "Enum16", "Time", "Time64":
		return driver.TypeString
	case "Date", "Date32", "DateTime", "DateTime32", "DateTime64":
		return driver.TypeTime
	case "Array", "Map", "Tuple", "Nested", "JSON", "Object", "Variant", "Dynamic",
		"Point", "Ring", "LineString", "MultiLineString", "Polygon", "MultiPolygon", "Geometry":
		return driver.TypeJSON
	case "Nothing":
		return driver.TypeNull
	default:
		if strings.HasPrefix(name, "Interval") {
			return driver.TypeInt
		}
		return driver.TypeUnknown
	}
}

// decimalScale reads S out of Decimal(P, S), Decimal32(S) and the like; ok
// is false when the type carries no scale.
func decimalScale(t string) (int32, bool) {
	_, params, ok := strings.Cut(t, "(")
	if !ok {
		return 0, false
	}
	params = strings.TrimSuffix(params, ")")
	if i := strings.LastIndexByte(params, ','); i >= 0 {
		params = params[i+1:]
	} else if baseName(t) == "Decimal" { // Decimal(P): scale 0
		return 0, true
	}
	s, err := strconv.Atoi(strings.TrimSpace(params))
	return int32(s), err == nil
}

// convertValue turns a value scanned by clickhouse-go into a driver.Value,
// guided by the column's native type. It never panics: a value it cannot
// model becomes JSON or, failing that, its text.
func convertValue(native string, v any) driver.Value {
	if b, isBig := v.(*big.Int); isBig && b != nil {
		return driver.Decimal(b.String())
	}
	v, ok := deref(v)
	if !ok {
		return driver.Null()
	}
	base := unwrapType(native)
	typ := columnType(native)
	if typ == driver.TypeJSON {
		return jsonOf(v)
	}
	switch x := v.(type) {
	case bool:
		return driver.Bool(x)
	case int8:
		return driver.Int(int64(x))
	case int16:
		return driver.Int(int64(x))
	case int32:
		return driver.Int(int64(x))
	case int64:
		return driver.Int(x)
	case int:
		return driver.Int(int64(x))
	case uint8:
		return driver.Int(int64(x))
	case uint16:
		return driver.Int(int64(x))
	case uint32:
		return driver.Int(int64(x))
	case uint64:
		if typ == driver.TypeDecimal { // a UInt64 column, whatever the value
			return driver.Decimal(strconv.FormatUint(x, 10))
		}
		if x > math.MaxInt64 {
			return driver.Decimal(strconv.FormatUint(x, 10))
		}
		return driver.Int(int64(x))
	case float32:
		return driver.Float(float64(x))
	case float64:
		return driver.Float(x)
	case string:
		if !utf8.ValidString(x) {
			return driver.Bytes([]byte(x))
		}
		return driver.String(x)
	case time.Time:
		return driver.Time(x)
	case decimal.Decimal:
		if s, ok := decimalScale(base); ok && s >= 0 {
			return driver.Decimal(x.StringFixed(s))
		}
		return driver.Decimal(x.String())
	case uuid.UUID:
		return driver.String(x.String())
	case net.IP:
		return driver.String(x.String())
	case netip.Addr:
		return driver.String(x.String())
	case time.Duration:
		if typ == driver.TypeString { // Time, Time64: a time of day that may pass 24 h
			return driver.String(timeOfDay(x))
		}
		return driver.Int(int64(x))
	case []byte:
		if typ == driver.TypeString {
			if utf8.Valid(x) {
				return driver.String(string(x))
			}
			return driver.Bytes(x)
		}
		return driver.Unknown(native, x, base64.StdEncoding.EncodeToString(x))
	case fmt.Stringer:
		return driver.String(x.String())
	}
	return jsonValue(v)
}

// deref follows pointers (and interfaces holding them) to the value. ok is
// false for nil.
func deref(v any) (any, bool) {
	if v == nil {
		return nil, false
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	return rv.Interface(), true
}

// jsonOf renders a value of a JSON column. It is total: a value that does
// not marshal (a NaN inside an Array) becomes a JSON string of its text, so
// the value's type always matches the column's.
func jsonOf(v any) driver.Value {
	out := jsonValue(v)
	if out.Type() == driver.TypeJSON {
		return out
	}
	raw, _ := json.Marshal(out.String())
	return driver.JSON(raw)
}

// jsonValue renders a composite value as JSON. Text that already is JSON
// (a JSON column scanned as a string) is kept as is.
func jsonValue(v any) driver.Value {
	switch x := v.(type) {
	case string:
		if json.Valid([]byte(x)) {
			return driver.JSON([]byte(x))
		}
		return driver.String(x)
	case []byte:
		if json.Valid(x) {
			return driver.JSON(x)
		}
	}
	// Marshal through a pointer, so pointer-receiver MarshalJSON methods
	// (clickhouse-go's JSON values) apply.
	p := reflect.New(reflect.TypeOf(v))
	p.Elem().Set(reflect.ValueOf(v))
	raw, err := json.Marshal(p.Interface())
	if err != nil {
		return driver.String(fmt.Sprint(v))
	}
	return driver.JSON(raw)
}

// timeOfDay renders a Time or Time64 value as ClickHouse does:
// [-]HH:MM:SS[.fraction], hours not wrapping at 24.
func timeOfDay(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign, d = "-", -d
	}
	h, m, s := d/time.Hour, d%time.Hour/time.Minute, d%time.Minute/time.Second
	out := fmt.Sprintf("%s%02d:%02d:%02d", sign, h, m, s)
	if frac := d % time.Second; frac != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%09d", frac), "0")
	}
	return out
}
