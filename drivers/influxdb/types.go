package influxdb

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/xavidop/tobira/driver"
)

// columnType is the tobira type of an Arrow column.
func columnType(dt arrow.DataType) driver.Type {
	switch dt.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
		arrow.UINT8, arrow.UINT16, arrow.UINT32:
		return driver.TypeInt
	case arrow.UINT64: // can exceed MaxInt64, so every value is a Decimal
		return driver.TypeDecimal
	case arrow.FLOAT16, arrow.FLOAT32, arrow.FLOAT64:
		return driver.TypeFloat
	case arrow.DECIMAL128, arrow.DECIMAL256, arrow.DECIMAL32, arrow.DECIMAL64:
		return driver.TypeDecimal
	case arrow.BOOL:
		return driver.TypeBool
	case arrow.STRING, arrow.LARGE_STRING, arrow.STRING_VIEW,
		arrow.DURATION, arrow.TIME32, arrow.TIME64:
		return driver.TypeString
	case arrow.BINARY, arrow.LARGE_BINARY, arrow.BINARY_VIEW, arrow.FIXED_SIZE_BINARY:
		return driver.TypeBytes
	case arrow.TIMESTAMP, arrow.DATE32, arrow.DATE64:
		return driver.TypeTime
	case arrow.LIST, arrow.LARGE_LIST, arrow.FIXED_SIZE_LIST, arrow.LIST_VIEW, arrow.LARGE_LIST_VIEW:
		return driver.TypeList
	case arrow.MAP: // a list of {key, value} entries
		return driver.TypeList
	case arrow.STRUCT:
		return driver.TypeObject
	case arrow.DICTIONARY:
		return columnType(dt.(*arrow.DictionaryType).ValueType)
	case arrow.NULL:
		return driver.TypeNull
	}
	return driver.TypeUnknown
}

// valueAt converts row i of arr. A type with no tobira equivalent becomes
// an Unknown carrying Arrow's display text; it never panics.
func valueAt(arr arrow.Array, i int) driver.Value {
	if arr.DataType().ID() == arrow.NULL || arr.IsNull(i) {
		return driver.Null()
	}
	switch a := arr.(type) {
	case *array.Int8:
		return driver.Int(int64(a.Value(i)))
	case *array.Int16:
		return driver.Int(int64(a.Value(i)))
	case *array.Int32:
		return driver.Int(int64(a.Value(i)))
	case *array.Int64:
		return driver.Int(a.Value(i))
	case *array.Uint8:
		return driver.Int(int64(a.Value(i)))
	case *array.Uint16:
		return driver.Int(int64(a.Value(i)))
	case *array.Uint32:
		return driver.Int(int64(a.Value(i)))
	case *array.Uint64:
		return driver.Decimal(strconv.FormatUint(a.Value(i), 10))
	case *array.Float16:
		return driver.Float(float64(a.Value(i).Float32()))
	case *array.Float32:
		return driver.Float(float64(a.Value(i)))
	case *array.Float64:
		return driver.Float(a.Value(i))
	case *array.Decimal128, *array.Decimal256, *array.Decimal32, *array.Decimal64:
		return driver.Decimal(arr.ValueStr(i))
	case *array.Boolean:
		return driver.Bool(a.Value(i))
	case *array.String:
		return driver.String(a.Value(i))
	case *array.LargeString:
		return driver.String(a.Value(i))
	case *array.StringView:
		return driver.String(a.Value(i))
	case *array.Binary:
		return driver.Bytes(a.Value(i))
	case *array.LargeBinary:
		return driver.Bytes(a.Value(i))
	case *array.BinaryView:
		return driver.Bytes(a.Value(i))
	case *array.FixedSizeBinary:
		return driver.Bytes(a.Value(i))
	case *array.Timestamp:
		unit := a.DataType().(*arrow.TimestampType).Unit
		return driver.Time(a.Value(i).ToTime(unit).UTC())
	case *array.Date32:
		return driver.Time(a.Value(i).ToTime().UTC())
	case *array.Date64:
		return driver.Time(a.Value(i).ToTime().UTC())
	case *array.Duration:
		return driver.String(durationText(int64(a.Value(i)), a.DataType().(*arrow.DurationType).Unit))
	case *array.Time32, *array.Time64:
		return driver.String(arr.ValueStr(i))
	case *array.Dictionary:
		return valueAt(a.Dictionary(), a.GetValueIndex(i))
	case array.ListLike:
		start, end := a.ValueOffsets(i)
		vals := a.ListValues()
		out := make([]driver.Value, 0, end-start)
		for j := start; j < end; j++ {
			out = append(out, valueAt(vals, int(j)))
		}
		return driver.List(out...)
	case *array.Struct:
		st := a.DataType().(*arrow.StructType)
		ms := make([]driver.Member, a.NumField())
		for f := range a.NumField() {
			ms[f] = driver.Member{Key: st.Field(f).Name, Value: valueAt(a.Field(f), i)}
		}
		return driver.ObjectValue(ms...)
	}
	raw, err := json.Marshal(arr.GetOneForMarshal(i))
	if err != nil {
		raw = nil
	}
	return driver.Unknown(arr.DataType().String(), raw, arr.ValueStr(i))
}

// durationText renders a duration of v units. When v in nanoseconds would
// overflow time.Duration (past about 292 years), it is shown in its own
// unit instead of wrapping around.
func durationText(v int64, unit arrow.TimeUnit) string {
	m := int64(unit.Multiplier())
	if v > math.MaxInt64/m || v < math.MinInt64/m {
		return strconv.FormatInt(v, 10) + []string{"s", "ms", "us", "ns"}[unit]
	}
	return time.Duration(v * m).String()
}

// fieldType is the tobira type of a column as information_schema spells its
// Arrow type ("Int64", "Dictionary(Int32, Utf8)", "Timestamp(ns)"…). It
// agrees with columnType, so Describe and Query give a column the same Type.
func fieldType(native string) driver.Type {
	n := native
	if strings.HasPrefix(n, "Dictionary(") {
		// Dictionary(<index>, <value>): the value type decides.
		if i := strings.IndexByte(n, ','); i >= 0 {
			n = strings.TrimSuffix(strings.TrimSpace(n[i+1:]), ")")
		}
	}
	switch {
	case n == "UInt64":
		return driver.TypeDecimal
	case isIntName(n):
		return driver.TypeInt
	case strings.HasPrefix(n, "Float"):
		return driver.TypeFloat
	case strings.HasPrefix(n, "Decimal"):
		return driver.TypeDecimal
	case n == "Boolean":
		return driver.TypeBool
	case n == "Utf8", n == "LargeUtf8", n == "Utf8View",
		strings.HasPrefix(n, "Duration"), strings.HasPrefix(n, "Time32"), strings.HasPrefix(n, "Time64"):
		return driver.TypeString
	case n == "Binary", n == "LargeBinary", n == "BinaryView", strings.HasPrefix(n, "FixedSizeBinary"):
		return driver.TypeBytes
	case strings.HasPrefix(n, "Timestamp"), strings.HasPrefix(n, "Date"):
		return driver.TypeTime
	case strings.HasPrefix(n, "List"), strings.HasPrefix(n, "LargeList"), strings.HasPrefix(n, "FixedSizeList"),
		strings.HasPrefix(n, "ListView"), strings.HasPrefix(n, "LargeListView"), strings.HasPrefix(n, "Map"):
		return driver.TypeList
	case strings.HasPrefix(n, "Struct"):
		return driver.TypeObject
	case n == "Null":
		return driver.TypeNull
	}
	return driver.TypeUnknown
}

// isIntName reports whether n names an integer type: Int8…Int64 or
// UInt8…UInt64 (and not, say, Interval).
func isIntName(n string) bool {
	n = strings.TrimPrefix(n, "U")
	return strings.HasPrefix(n, "Int") && len(n) > 3 && n[3] >= '0' && n[3] <= '9'
}
