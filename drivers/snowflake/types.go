package snowflake

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// Snowflake types map to tobira types like this (columnType):
//
//   - NUMBER/FIXED with scale 0: Int; a value past int64 is Decimal;
//   - NUMBER with a scale, DECFLOAT: Decimal, as exact text (gosnowflake
//     hands NUMBER(p,s) over as its decimal string, trailing zeros kept);
//   - FLOAT/REAL: Float; BOOLEAN: Bool; TEXT/VARCHAR: String;
//   - DATE, TIMESTAMP_NTZ/LTZ/TZ: Time (an NTZ value is read as UTC);
//   - TIME: String (HH:MM:SS[.fraction]);
//   - VARIANT, OBJECT, ARRAY, MAP: JSON (a value that is not valid JSON is a
//     String);
//   - BINARY: Bytes; GEOGRAPHY, GEOMETRY: String (GeoJSON or WKT, as the
//     session's output format says);
//   - VECTOR(FLOAT, n): Vector; VECTOR(INT, n): a List of Int.
//
// A TEXT, VARIANT/OBJECT/ARRAY or BINARY value longer than maxValueBytes is
// cut (text at a UTF-8 boundary; cut JSON becomes String) and the result
// carries cutWarning. A VARIANT can hold up to 128 MB.

// maxValueBytes caps one TEXT, semi-structured or BINARY value: 1 MiB, as
// the other drivers cap LOBs. gosnowflake has already decoded the chunk, so
// this bounds what tobira keeps and shows.
const maxValueBytes = 1 << 20

// cutWarning is the Stats warning of a result with a cut value.
const cutWarning = "some values were longer than 1 MiB and were cut; select a substring (SUBSTR) to see the rest"

// columnType normalises a Snowflake type name: a result column's
// DatabaseTypeName (FIXED, REAL, TEXT …) or a catalog data type (NUMBER,
// FLOAT, VARCHAR …), with its scale.
func columnType(native string, scale int64) driver.Type {
	switch strings.ToUpper(native) {
	case "FIXED", "NUMBER", "DECIMAL", "NUMERIC", "INT", "INTEGER", "BIGINT", "SMALLINT", "TINYINT", "BYTEINT":
		if scale == 0 {
			return driver.TypeInt
		}
		return driver.TypeDecimal
	case "DECFLOAT":
		return driver.TypeDecimal
	case "REAL", "FLOAT", "FLOAT4", "FLOAT8", "DOUBLE", "DOUBLE PRECISION":
		return driver.TypeFloat
	case "TEXT", "VARCHAR", "STRING", "CHAR", "CHARACTER", "NCHAR", "NVARCHAR":
		return driver.TypeString
	case "BOOLEAN":
		return driver.TypeBool
	case "DATE", "DATETIME", "TIMESTAMP", "TIMESTAMP_NTZ", "TIMESTAMP_LTZ", "TIMESTAMP_TZ":
		return driver.TypeTime
	case "TIME", "GEOGRAPHY", "GEOMETRY":
		return driver.TypeString
	case "VARIANT", "OBJECT", "ARRAY", "MAP":
		return driver.TypeJSON
	case "BINARY", "VARBINARY":
		return driver.TypeBytes
	case "VECTOR":
		return driver.TypeVector
	}
	return driver.TypeUnknown
}

// convertValue turns one value gosnowflake scanned into a driver.Value. cut
// is set when the value was cut at maxValueBytes.
func convertValue(native string, scale int64, raw any, cut *bool) driver.Value {
	if raw == nil {
		return driver.Null()
	}
	typ := columnType(native, scale)
	switch x := raw.(type) {
	case string:
		return convertString(native, typ, x, cut)
	case bool:
		return driver.Bool(x)
	case int64:
		if typ == driver.TypeDecimal {
			return driver.Decimal(scaled(strconv.FormatInt(x, 10), scale))
		}
		return driver.Int(x)
	case float64:
		if typ == driver.TypeDecimal || typ == driver.TypeInt {
			return driver.Decimal(strconv.FormatFloat(x, 'f', int(scale), 64))
		}
		return driver.Float(x)
	case *big.Int:
		if x.IsInt64() {
			return driver.Int(x.Int64())
		}
		return driver.Decimal(x.String())
	case *big.Float:
		return driver.Decimal(x.Text('f', int(scale)))
	case time.Time:
		if strings.EqualFold(native, "TIME") {
			return driver.String(timeOfDay(x))
		}
		return driver.Time(x)
	case []byte:
		if len(x) > maxValueBytes {
			*cut = true
			x = x[:maxValueBytes]
		}
		return driver.Bytes(x)
	case []float32:
		return driver.Vector(x)
	case []int32:
		out := make([]driver.Value, len(x))
		for i, n := range x {
			out[i] = driver.Int(int64(n))
		}
		return driver.List(out...)
	}
	s := fmt.Sprint(raw)
	return driver.Unknown(native, []byte(s), s)
}

// convertString converts a value gosnowflake hands over as text: numbers,
// strings, semi-structured values and, in JSON results, booleans.
func convertString(native string, typ driver.Type, s string, cut *bool) driver.Value {
	switch typ {
	case driver.TypeInt:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return driver.Int(n)
		}
		return driver.Decimal(s)
	case driver.TypeDecimal:
		return driver.Decimal(s)
	case driver.TypeFloat:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return driver.Float(f)
		}
	case driver.TypeBool:
		if b, err := strconv.ParseBool(s); err == nil {
			return driver.Bool(b)
		}
	case driver.TypeJSON:
		if len(s) > maxValueBytes {
			*cut = true
			return driver.String(cutUTF8(s, maxValueBytes))
		}
		if json.Valid([]byte(s)) {
			return driver.JSON([]byte(s))
		}
		return driver.String(s)
	case driver.TypeString, driver.TypeUnknown:
		if len(s) > maxValueBytes {
			*cut = true
			return driver.String(cutUTF8(s, maxValueBytes))
		}
		return driver.String(s)
	}
	if len(s) > maxValueBytes {
		*cut = true
		s = cutUTF8(s, maxValueBytes)
	}
	return driver.Unknown(native, []byte(s), s)
}

// scaled places a decimal point scale digits from the right of an integer
// string.
func scaled(digits string, scale int64) string {
	if scale <= 0 {
		return digits
	}
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	for int64(len(digits)) <= scale {
		digits = "0" + digits
	}
	out := digits[:int64(len(digits))-scale] + "." + digits[int64(len(digits))-scale:]
	if neg {
		out = "-" + out
	}
	return out
}

// timeOfDay renders a TIME value: HH:MM:SS, then the fraction without
// trailing zeros.
func timeOfDay(t time.Time) string { return t.Format("15:04:05.999999999") }

// cutUTF8 cuts s to at most n bytes, at a UTF-8 boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// bindValue converts a parameter to what gosnowflake binds. Decimals and
// JSON travel as text, which the server casts to the column's type; a time
// travels as RFC 3339 text in UTC, so its instant survives whatever the
// session's TIMEZONE is.
func bindValue(v driver.Value) (any, error) {
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
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format(time.RFC3339Nano), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return string(raw), nil
	}
	return nil, fmt.Errorf("a %s value cannot be bound", v.Type())
}

// bindArgs converts query parameters.
func bindArgs(op string, params []driver.Value) ([]any, error) {
	args := make([]any, len(params))
	for i, p := range params {
		a, err := bindValue(p)
		if err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("parameter %d: %w", i+1, err))
		}
		args[i] = a
	}
	return args, nil
}
