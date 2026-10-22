package bigquery

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	bq "cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"

	"github.com/xavidop/tobira/driver"
)

// Types map as follows:
//
//   - INT64: Int; FLOAT64: Float; BOOL: Bool; NUMERIC, BIGNUMERIC: Decimal
//     (exact);
//   - STRING: String; BYTES: Bytes; JSON: JSON;
//   - TIMESTAMP: Time in UTC; DATE and DATETIME: Time in UTC (a DATETIME
//     has no zone; its wall time is read as UTC);
//   - TIME, GEOGRAPHY (WKT), INTERVAL and RANGE: String;
//   - ARRAY (a REPEATED field): List; STRUCT (RECORD): Object, fields in
//     schema order.
//
// One value holding more than maxListElems array elements in all (nested
// arrays counted) is an Unknown placeholder instead, and a STRING, BYTES,
// JSON or GEOGRAPHY value longer than maxValueBytes is cut (text at a UTF-8
// boundary; cut JSON becomes String), which the result reports as a
// warning.

// maxListElems caps the array elements read into one value (a variable so
// tests can lower it).
var maxListElems = 10000

// maxValueBytes caps one STRING, BYTES, JSON or GEOGRAPHY value: 1 MiB, as
// the other drivers cap LOBs. The client has already decoded the page (at
// most about 10 MB), so this bounds what tobira keeps and shows.
var maxValueBytes = 1 << 20

// cutWarning is the Stats warning of a result with a cut value.
const cutWarning = "some values were longer than 1 MiB and were cut; select a substring (SUBSTR) to see the rest"

// fieldType normalises a field's type.
func fieldType(fs *bq.FieldSchema) driver.Type {
	if fs.Repeated {
		return driver.TypeList
	}
	switch fs.Type {
	case bq.IntegerFieldType:
		return driver.TypeInt
	case bq.FloatFieldType:
		return driver.TypeFloat
	case bq.NumericFieldType, bq.BigNumericFieldType:
		return driver.TypeDecimal
	case bq.BooleanFieldType:
		return driver.TypeBool
	case bq.StringFieldType, bq.TimeFieldType, bq.GeographyFieldType, bq.IntervalFieldType, bq.RangeFieldType:
		return driver.TypeString
	case bq.BytesFieldType:
		return driver.TypeBytes
	case bq.TimestampFieldType, bq.DateFieldType, bq.DateTimeFieldType:
		return driver.TypeTime
	case bq.JSONFieldType:
		return driver.TypeJSON
	case bq.RecordFieldType:
		return driver.TypeObject
	}
	return driver.TypeUnknown
}

// nativeType spells a field's type in GoogleSQL: "INT64", "NUMERIC(10, 2)",
// "ARRAY<STRUCT<a STRING>>".
func nativeType(fs *bq.FieldSchema) string {
	t := scalarNative(fs)
	if fs.Repeated {
		return "ARRAY<" + t + ">"
	}
	return t
}

func scalarNative(fs *bq.FieldSchema) string {
	switch fs.Type {
	case bq.IntegerFieldType:
		return "INT64"
	case bq.FloatFieldType:
		return "FLOAT64"
	case bq.BooleanFieldType:
		return "BOOL"
	case bq.NumericFieldType, bq.BigNumericFieldType:
		if fs.Precision > 0 {
			if fs.Scale > 0 {
				return fmt.Sprintf("%s(%d, %d)", fs.Type, fs.Precision, fs.Scale)
			}
			return fmt.Sprintf("%s(%d)", fs.Type, fs.Precision)
		}
		return string(fs.Type)
	case bq.StringFieldType, bq.BytesFieldType:
		if fs.MaxLength > 0 {
			return fmt.Sprintf("%s(%d)", fs.Type, fs.MaxLength)
		}
		return string(fs.Type)
	case bq.RecordFieldType:
		parts := make([]string, len(fs.Schema))
		for i, f := range fs.Schema {
			parts[i] = f.Name + " " + nativeType(f)
		}
		return "STRUCT<" + strings.Join(parts, ", ") + ">"
	case bq.RangeFieldType:
		if fs.RangeElementType != nil {
			return "RANGE<" + string(fs.RangeElementType.Type) + ">"
		}
	}
	return string(fs.Type)
}

// toValue converts one value the client read for field fs.
func toValue(fs *bq.FieldSchema, v bq.Value) driver.Value {
	val, _ := toValueCut(fs, v)
	return val
}

// toValueCut is toValue, also reporting whether a value (or one inside an
// ARRAY or STRUCT) was cut at maxValueBytes.
func toValueCut(fs *bq.FieldSchema, v bq.Value) (driver.Value, bool) {
	if v == nil {
		return driver.Null(), false
	}
	if n := countElems(fs, v, 0); n > maxListElems {
		return driver.Unknown(nativeType(fs), nil, fmt.Sprintf(
			"<%s holding %d elements: too large to show (more than %d); select fewer elements, for example with ARRAY_LENGTH or a subscript>",
			nativeType(fs), n, maxListElems)), false
	}
	var cut bool
	return convert(fs, v, &cut), cut
}

// cutUTF8 shortens s to at most n bytes, at a UTF-8 boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// countElems counts the array elements under v, stopping once past the
// cap.
func countElems(fs *bq.FieldSchema, v bq.Value, n int) int {
	if fs.Repeated {
		vs, _ := v.([]bq.Value)
		n += len(vs)
		if fs.Type == bq.RecordFieldType {
			for _, e := range vs {
				if n > maxListElems {
					return n
				}
				n = countStruct(fs, e, n)
			}
		}
		return n
	}
	if fs.Type == bq.RecordFieldType {
		return countStruct(fs, v, n)
	}
	return n
}

func countStruct(fs *bq.FieldSchema, v bq.Value, n int) int {
	vs, _ := v.([]bq.Value)
	for i, f := range fs.Schema {
		if i >= len(vs) || n > maxListElems {
			break
		}
		if vs[i] != nil {
			n = countElems(f, vs[i], n)
		}
	}
	return n
}

func convert(fs *bq.FieldSchema, v bq.Value, cut *bool) driver.Value {
	if v == nil {
		return driver.Null()
	}
	if fs.Repeated {
		vs, ok := v.([]bq.Value)
		if !ok {
			return unknown(fs, v)
		}
		elem := *fs
		elem.Repeated = false
		out := make([]driver.Value, len(vs))
		for i, e := range vs {
			out[i] = convert(&elem, e, cut)
		}
		return driver.List(out...)
	}
	switch x := v.(type) {
	case int64:
		return driver.Int(x)
	case float64:
		return driver.Float(x)
	case bool:
		return driver.Bool(x)
	case []byte:
		if len(x) > maxValueBytes {
			*cut = true
			x = x[:maxValueBytes]
		}
		return driver.Bytes(x)
	case *big.Rat:
		return driver.Decimal(ratString(x))
	case time.Time:
		return driver.Time(x.UTC())
	case civil.Date:
		return driver.Time(x.In(time.UTC))
	case civil.DateTime:
		return driver.Time(x.In(time.UTC))
	case civil.Time:
		return driver.String(bq.CivilTimeString(x))
	case *bq.IntervalValue:
		return driver.String(x.String())
	case *bq.RangeValue:
		return driver.String(rangeString(fs, x))
	case string:
		if len(x) > maxValueBytes {
			*cut = true
			return driver.String(cutUTF8(x, maxValueBytes))
		}
		if fs.Type == bq.JSONFieldType {
			if json.Valid([]byte(x)) {
				return driver.JSON([]byte(x))
			}
		}
		return driver.String(x)
	case []bq.Value:
		if fs.Type != bq.RecordFieldType {
			return unknown(fs, v)
		}
		obj := make(driver.Object, 0, len(fs.Schema))
		for i, f := range fs.Schema {
			var e bq.Value
			if i < len(x) {
				e = x[i]
			}
			obj = append(obj, driver.Member{Key: f.Name, Value: convert(f, e, cut)})
		}
		return driver.ObjectValue(obj...)
	}
	return unknown(fs, v)
}

func unknown(fs *bq.FieldSchema, v bq.Value) driver.Value {
	s := fmt.Sprint(v)
	return driver.Unknown(nativeType(fs), []byte(s), s)
}

// ratString renders an exact decimal without trailing zeros: 12.5, 3.
func ratString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	s := r.FloatString(bq.BigNumericScaleDigits)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// rangeString renders a RANGE as GoogleSQL prints it: [start, end), an
// unbounded end as UNBOUNDED.
func rangeString(fs *bq.FieldSchema, r *bq.RangeValue) string {
	elem := &bq.FieldSchema{Type: bq.DateFieldType}
	if fs.RangeElementType != nil {
		elem.Type = fs.RangeElementType.Type
	}
	end := func(v bq.Value) string {
		if v == nil {
			return "UNBOUNDED"
		}
		var cut bool
		x := convert(elem, v, &cut)
		if t, ok := x.AsTime(); ok {
			switch elem.Type {
			case bq.DateFieldType:
				return t.Format(time.DateOnly)
			case bq.DateTimeFieldType:
				return t.Format("2006-01-02T15:04:05.999999")
			}
			return t.Format(time.RFC3339Nano)
		}
		return x.String()
	}
	return "[" + end(r.Start) + ", " + end(r.End) + ")"
}
