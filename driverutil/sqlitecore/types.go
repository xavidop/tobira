package sqlitecore

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

var timeLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02",
}

// baseType returns the upper-cased declared type without its "(…)" suffix.
func baseType(declared string) string {
	d := strings.ToUpper(strings.TrimSpace(declared))
	if i := strings.IndexByte(d, '('); i >= 0 {
		d = strings.TrimSpace(d[:i])
	}
	return d
}

// MapType maps a declared column type to a driver.Type. An empty or unknown
// declared type is String; ColumnType refines the empty case from a value.
func MapType(declared string) driver.Type {
	switch baseType(declared) {
	case "INTEGER", "INT", "BIGINT", "SMALLINT", "TINYINT", "MEDIUMINT", "INT2", "INT8":
		return driver.TypeInt
	case "REAL", "DOUBLE", "DOUBLE PRECISION", "FLOAT":
		return driver.TypeFloat
	case "DECIMAL", "NUMERIC":
		return driver.TypeDecimal
	case "BOOLEAN", "BOOL":
		return driver.TypeBool
	case "DATE", "DATETIME", "TIMESTAMP":
		return driver.TypeTime
	case "JSON":
		return driver.TypeJSON
	case "BLOB":
		return driver.TypeBytes
	}
	return driver.TypeString
}

// ColumnType is the Column.Type for a declared type; with no declared type it
// is derived from the first runtime value.
func ColumnType(declared string, sample any) driver.Type {
	if strings.TrimSpace(declared) != "" {
		return MapType(declared)
	}
	switch sample.(type) {
	case int64:
		return driver.TypeInt
	case float64:
		return driver.TypeFloat
	case []byte:
		return driver.TypeBytes
	}
	return driver.TypeString
}

// decimalScale extracts s from "DECIMAL(p,s)"; -1 when absent.
func decimalScale(declared string) int {
	i, j := strings.IndexByte(declared, '('), strings.IndexByte(declared, ')')
	if i < 0 || j < i {
		return -1
	}
	_, s, ok := strings.Cut(declared[i+1:j], ",")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// ConvertValue converts a database/sql scanned value using the declared type.
func ConvertValue(declared string, v any) driver.Value {
	if v == nil {
		return driver.Null()
	}
	return ConvertTyped(declared, ColumnType(declared, v), v)
}

// ConvertTyped is ConvertValue with the column type already decided.
func ConvertTyped(declared string, typ driver.Type, v any) driver.Value {
	if v == nil {
		return driver.Null()
	}
	switch x := v.(type) {
	case int64:
		return convertInt(declared, typ, x)
	case float64:
		switch typ {
		case driver.TypeDecimal:
			return driver.Decimal(formatDecimal(declared, x))
		case driver.TypeFloat:
			return driver.Float(x)
		}
		return driver.Float(x)
	case []byte:
		if typ == driver.TypeBytes {
			return driver.Bytes(x)
		}
		return convertText(declared, typ, string(x))
	case string:
		return convertText(declared, typ, x)
	case time.Time:
		return driver.Time(x)
	case bool:
		return driver.Bool(x)
	}
	return driver.String("")
}

func convertInt(declared string, typ driver.Type, x int64) driver.Value {
	switch typ {
	case driver.TypeBool:
		if x == 0 || x == 1 {
			return driver.Bool(x == 1)
		}
	case driver.TypeDecimal:
		return driver.Decimal(formatDecimal(declared, float64(x), strconv.FormatInt(x, 10)))
	case driver.TypeTime:
		return driver.Time(time.Unix(x, 0).UTC())
	}
	return driver.Int(x)
}

func convertText(declared string, typ driver.Type, s string) driver.Value {
	switch typ {
	case driver.TypeDecimal:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return driver.Decimal(decimalText(declared, s, f))
		}
	case driver.TypeTime:
		for _, l := range timeLayouts {
			if t, err := time.Parse(l, s); err == nil {
				return driver.Time(t)
			}
		}
	case driver.TypeJSON:
		if json.Valid([]byte(s)) {
			return driver.JSON([]byte(s))
		}
	}
	return driver.String(s)
}

// formatDecimal renders f with the declared scale, or shortest form without
// one. An optional exact integer text avoids float rounding for big integers
// when no scale is declared.
func formatDecimal(declared string, f float64, exact ...string) string {
	if scale := decimalScale(declared); scale >= 0 {
		if len(exact) > 0 && scale > 0 {
			return exact[0] + "." + strings.Repeat("0", scale)
		}
		return strconv.FormatFloat(f, 'f', scale, 64)
	}
	if len(exact) > 0 {
		return exact[0]
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// decimalText applies the declared scale to numeric text: plain decimals are
// zero-padded (never truncated), anything else is re-formatted from f.
func decimalText(declared, s string, f float64) string {
	scale := decimalScale(declared)
	if scale < 0 {
		return s
	}
	body := strings.TrimLeft(s, "+-")
	plain := body != "" && strings.Count(body, ".") <= 1 && strings.Trim(body, "0123456789.") == "" && body != "."
	if !plain {
		return strconv.FormatFloat(f, 'f', scale, 64)
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) >= scale {
		return s
	}
	if scale > 0 {
		return whole + "." + frac + strings.Repeat("0", scale-len(frac))
	}
	return whole
}
