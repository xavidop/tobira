package mysql

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// columnType maps a result column's type, as go-sql-driver names it
// (ColumnType.DatabaseTypeName), to tobira's Type. go-sql-driver does not
// expose a column's display width, so TINYINT(1) in a result set is Int, not
// Bool; Describe, which reads information_schema, reports it as Bool. TIME is
// a duration that can exceed a day, so it stays String.
func columnType(dbType string) driver.Type {
	switch strings.TrimPrefix(dbType, "UNSIGNED ") {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "BIGINT", "YEAR", "BIT":
		return driver.TypeInt
	case "DECIMAL":
		return driver.TypeDecimal
	case "FLOAT", "DOUBLE":
		return driver.TypeFloat
	case "DATE", "DATETIME", "TIMESTAMP":
		return driver.TypeTime
	case "JSON":
		return driver.TypeJSON
	case "CHAR", "VARCHAR", "TEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT", "ENUM", "SET", "TIME":
		return driver.TypeString
	case "BINARY", "VARBINARY", "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "GEOMETRY":
		return driver.TypeBytes
	}
	return driver.TypeUnknown
}

// geometryTypes are the information_schema DATA_TYPE names of spatial columns.
var geometryTypes = map[string]bool{
	"geometry": true, "point": true, "linestring": true, "polygon": true, "multipoint": true,
	"multilinestring": true, "multipolygon": true, "geometrycollection": true, "geomcollection": true,
}

// fieldType maps an information_schema column (DATA_TYPE and COLUMN_TYPE) to
// tobira's Type. TINYINT(1), MySQL's BOOL, is Bool.
func fieldType(dataType, colType string) driver.Type {
	dt := strings.ToLower(dataType)
	switch {
	case dt == "tinyint" && strings.HasPrefix(strings.ToLower(colType), "tinyint(1)"):
		return driver.TypeBool
	case geometryTypes[dt]:
		return driver.TypeBytes
	case dt == "uuid" || dt == "inet4" || dt == "inet6": // MariaDB's native types, read as text
		return driver.TypeString
	}
	return columnType(strings.ToUpper(dt))
}

// convertValue converts a value scanned from a column of type dbType. Values
// arrive typed from both protocols (int64, uint64, float32/64, time.Time) or
// as raw text/bytes. BIGINT UNSIGNED and BIT values above MaxInt64 become
// Decimal; times are UTC.
func convertValue(dbType string, v any) driver.Value {
	switch x := v.(type) {
	case nil:
		return driver.Null()
	case int64:
		return driver.Int(x)
	case uint64:
		return fromUint64(x)
	case float64:
		return driver.Float(x)
	case float32:
		// Keep the shortest decimal form of the float32 (0.1, not 0.10000000149).
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		return driver.Float(f)
	case bool:
		return driver.Bool(x)
	case time.Time:
		return driver.Time(x.UTC())
	case string:
		return convertBytes(dbType, []byte(x))
	case []byte:
		return convertBytes(dbType, x)
	}
	return driver.Unknown(dbType, nil, fmt.Sprint(v))
}

func fromUint64(u uint64) driver.Value {
	if u <= math.MaxInt64 {
		return driver.Int(int64(u))
	}
	return driver.Decimal(strconv.FormatUint(u, 10))
}

// convertBytes converts a raw text or binary value by its column type. Text
// that does not parse as the column's type is kept as a String.
func convertBytes(dbType string, b []byte) driver.Value {
	if strings.TrimPrefix(dbType, "UNSIGNED ") == "BIT" && len(b) <= 8 {
		var buf [8]byte
		copy(buf[8-len(b):], b)
		return fromUint64(binary.BigEndian.Uint64(buf[:]))
	}
	s := string(b)
	switch columnType(dbType) {
	case driver.TypeBytes:
		return driver.Bytes(b)
	case driver.TypeJSON:
		return driver.JSON(b)
	case driver.TypeDecimal:
		return driver.Decimal(s)
	case driver.TypeInt:
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return driver.Int(i)
		}
		if u, err := strconv.ParseUint(s, 10, 64); err == nil {
			return fromUint64(u)
		}
	case driver.TypeFloat:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return driver.Float(f)
		}
	case driver.TypeTime:
		for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
				return driver.Time(t)
			}
		}
	case driver.TypeUnknown:
		return driver.Unknown(dbType, b, s)
	}
	return driver.String(s)
}

var errUnsupportedParam = errors.New("this value type cannot be a MySQL parameter")

// toArg converts a query parameter to what go-sql-driver binds. Decimals
// travel as text so they stay exact, JSON as its text, and times in UTC.
func toArg(v driver.Value) (any, error) {
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
		return f, nil
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC(), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return string(raw), nil
	}
	return nil, fmt.Errorf("%w: %s", errUnsupportedParam, v.Type())
}
