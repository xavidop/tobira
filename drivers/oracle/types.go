package oracle

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	go_ora "github.com/sijms/go-ora/v2"

	"github.com/xavidop/tobira/driver"
)

// maxLOBBytes caps a CLOB, NCLOB, BLOB, LONG, LONG RAW or JSON value kept in
// a result row. go-ora reads LOBs inline with their row, so the cap bounds
// what tobira holds and shows, not what crosses the network; a longer value
// is cut (at a UTF-8 boundary for text) and the result carries a warning.
const maxLOBBytes = 1 << 20

// numKind says how a NUMBER result column's values convert.
type numKind uint8

const (
	numNone          numKind = iota // not a NUMBER
	numInt                          // NUMBER(p) with p <= 18: Int
	numDecimal                      // NUMBER(p,s) with s > 0, or p > 18: Decimal with s digits
	numUnconstrained                // NUMBER, FLOAT or an expression: Int when integral and in range, else Decimal
)

// colInfo is how one result column converts.
type colInfo struct {
	db     string // go-ora's type name (ColumnType.DatabaseTypeName)
	native string // the Oracle spelling, for Column.NativeType
	typ    driver.Type
	num    numKind
	scale  int  // digits after the point, numDecimal only
	lob    bool // values are capped at maxLOBBytes
	// keepZone keeps a time's zone as go-ora reports it (TIMESTAMP WITH
	// TIME ZONE: a fixed offset, or a region); other times are UTC.
	keepZone bool
}

// resultColumn maps a result column, as go-ora describes it, to how its
// values convert. go-ora reports NUMBER precision and scale (38 and 255 for
// an unconstrained NUMBER, FLOAT or an expression); BOOLEAN columns of 23ai
// reach it as such a NUMBER, so their values come back as Int 1 or 0. CLOB
// and LONG are LongVarChar, BLOB and LONG RAW are LongRaw, and JSON is
// OCIBlobLocator carrying the JSON text.
func resultColumn(db string, precision, scale int64, hasPS bool) colInfo {
	ci := colInfo{db: db, native: db, typ: driver.TypeUnknown}
	switch db {
	case "NUMBER":
		ci.native = "NUMBER"
		switch {
		case !hasPS || scale == 255 || scale < 0:
			ci.typ, ci.num = driver.TypeDecimal, numUnconstrained
		case scale == 0 && precision > 0 && precision <= 18:
			ci.typ, ci.num = driver.TypeInt, numInt
			ci.native = fmt.Sprintf("NUMBER(%d)", precision)
		case scale == 0:
			ci.typ, ci.num = driver.TypeDecimal, numDecimal
			ci.native = fmt.Sprintf("NUMBER(%d)", precision)
		default:
			ci.typ, ci.num, ci.scale = driver.TypeDecimal, numDecimal, int(scale)
			ci.native = fmt.Sprintf("NUMBER(%d,%d)", precision, scale)
		}
	case "IBFloat":
		ci.typ, ci.native = driver.TypeFloat, "BINARY_FLOAT"
	case "IBDouble":
		ci.typ, ci.native = driver.TypeFloat, "BINARY_DOUBLE"
	case "NCHAR":
		ci.typ, ci.native = driver.TypeString, "VARCHAR2"
	case "CHAR", "ROWID", "UROWID":
		ci.typ = driver.TypeString
	case "LongVarChar":
		ci.typ, ci.native, ci.lob = driver.TypeString, "CLOB", true
	case "LONG":
		ci.typ, ci.lob = driver.TypeString, true
	case "RAW":
		ci.typ = driver.TypeBytes
	case "LongRaw":
		ci.typ, ci.native, ci.lob = driver.TypeBytes, "BLOB", true
	case "OCIBlobLocator":
		ci.typ, ci.native, ci.lob = driver.TypeJSON, "JSON", true
	case "DATE":
		ci.typ = driver.TypeTime
	case "TimeStampDTY", "TIMESTAMP":
		ci.typ, ci.native = driver.TypeTime, "TIMESTAMP"
	case "TimeStampTZ_DTY", "TIMESTAMPTZ":
		ci.typ, ci.native, ci.keepZone = driver.TypeTime, "TIMESTAMP WITH TIME ZONE", true
	case "TimeStampLTZ_DTY", "TimeStampeLTZ":
		ci.typ, ci.native = driver.TypeTime, "TIMESTAMP WITH LOCAL TIME ZONE"
	case "IntervalDS_DTY", "IntervalDS":
		ci.typ, ci.native = driver.TypeString, "INTERVAL DAY TO SECOND"
	case "IntervalYM_DTY", "IntervalYM":
		ci.typ, ci.native = driver.TypeString, "INTERVAL YEAR TO MONTH"
	case go_ora.VECTOR.String():
		ci.typ, ci.native = driver.TypeVector, "VECTOR"
	}
	return ci
}

// fieldType maps an ALL_TAB_COLS column (DATA_TYPE, DATA_PRECISION,
// DATA_SCALE) to tobira's Type. NUMBER(p) with p <= 18, and INTEGER (a NUMBER
// with scale 0 and no precision), are Int; other NUMBERs and FLOAT are
// Decimal, as their values are exact.
func fieldType(dataType string, precision, scale sql.NullInt64) driver.Type {
	dt := strings.ToUpper(dataType)
	switch {
	case dt == "NUMBER":
		if scale.Valid && scale.Int64 == 0 && (!precision.Valid || precision.Int64 <= 18) {
			return driver.TypeInt
		}
		return driver.TypeDecimal
	case dt == "FLOAT":
		return driver.TypeDecimal
	case dt == "BINARY_FLOAT", dt == "BINARY_DOUBLE":
		return driver.TypeFloat
	case dt == "BOOLEAN":
		return driver.TypeBool
	case dt == "JSON":
		return driver.TypeJSON
	case dt == "VECTOR":
		return driver.TypeVector
	case dt == "DATE", strings.HasPrefix(dt, "TIMESTAMP"):
		return driver.TypeTime
	case dt == "RAW", dt == "LONG RAW", dt == "BLOB":
		return driver.TypeBytes
	case strings.HasPrefix(dt, "INTERVAL"):
		return driver.TypeString
	}
	switch dt {
	case "VARCHAR2", "NVARCHAR2", "VARCHAR", "CHAR", "NCHAR", "CLOB", "NCLOB", "LONG", "ROWID", "UROWID":
		return driver.TypeString
	}
	return driver.TypeUnknown
}

// convertValue converts a value scanned from column ci. NUMBER values arrive
// as text and are typed by the column; TIMESTAMP WITH TIME ZONE keeps its
// zone and other times are UTC; LOB values over
// maxLOBBytes are cut, which truncated reports.
func convertValue(ci colInfo, v any) (val driver.Value, truncated bool) {
	switch x := v.(type) {
	case nil:
		return driver.Null(), false
	case string:
		if ci.num != numNone {
			return convertNumber(ci, x), false
		}
		if ci.lob && len(x) > maxLOBBytes {
			return driver.String(cutUTF8(x, maxLOBBytes)), true
		}
		return driver.String(x), false
	case []byte:
		if ci.lob && len(x) > maxLOBBytes {
			if ci.typ == driver.TypeJSON {
				return driver.String(cutUTF8(string(x), maxLOBBytes)), true
			}
			return driver.Bytes(x[:maxLOBBytes]), true
		}
		if ci.typ == driver.TypeJSON && json.Valid(x) {
			return driver.JSON(x), false
		}
		return driver.Bytes(x), false
	case int64:
		return driver.Int(x), false
	case float64:
		return driver.Float(x), false
	case float32:
		// Keep the shortest decimal form of the float32 (0.1, not 0.10000000149).
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		return driver.Float(f), false
	case bool:
		return driver.Bool(x), false
	case time.Time:
		if ci.keepZone {
			return driver.Time(x), false
		}
		return driver.Time(x.UTC()), false
	case []float32:
		return driver.Vector(x), false
	case []float64:
		out := make([]float32, len(x))
		for i, f := range x {
			out[i] = float32(f)
		}
		return driver.Vector(out), false
	}
	return driver.Unknown(ci.native, nil, fmt.Sprint(v)), false
}

// convertNumber types the text of a NUMBER value by its column.
func convertNumber(ci colInfo, s string) driver.Value {
	switch ci.num {
	case numInt:
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return driver.Int(i)
		}
	case numUnconstrained:
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return driver.Int(i)
		}
	case numDecimal:
		if ci.scale > 0 {
			if r, ok := new(big.Rat).SetString(s); ok {
				return driver.Decimal(r.FloatString(ci.scale))
			}
		}
	}
	return driver.Decimal(s)
}

// cutUTF8 returns at most n bytes of s, ending on a rune boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var errUnsupportedParam = errors.New("this value type cannot be an Oracle parameter")

// toArg converts a query parameter to what go-ora binds. Decimals travel as
// Oracle NUMBERs so they stay exact whatever the session's NLS settings,
// Bools as 1 or 0 (which BOOLEAN and NUMBER(1) columns both accept), JSON as
// its text, and times in UTC.
func toArg(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		if b, _ := v.AsBool(); b {
			return int64(1), nil
		}
		return int64(0), nil
	case driver.TypeInt:
		i, _ := v.AsInt()
		return i, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		if !numericLiteral.MatchString(s) {
			return nil, fmt.Errorf("decimal %q is not a number", s)
		}
		n, err := go_ora.NewNumberFromString(s)
		if err != nil || n == nil {
			return nil, fmt.Errorf("decimal %q is not a number", s)
		}
		return n, nil
	case driver.TypeString:
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
