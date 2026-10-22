package vertica

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// Vertica types map to tobira types like this (columnType):
//
//   - INT (every integer type): Int; FLOAT: Float; BOOLEAN: Bool;
//   - NUMERIC/DECIMAL/NUMBER/MONEY: Decimal. vertica-sql-go decodes NUMERIC
//     through float64, so a value with more than 15 significant digits
//     loses precision in a Query result; the column's scale restores the
//     trailing zeros. Browse reads NUMERIC columns wider than 15 digits as
//     text, exactly. Cast to VARCHAR in a query to see such a value exactly;
//   - CHAR, VARCHAR, LONG VARCHAR, UUID: String; INTERVAL: String;
//   - DATE, TIMESTAMP, TIMESTAMPTZ: Time (a TIMESTAMP's wall clock is read
//     as UTC); TIME, TIMETZ: String (HH:MM:SS[.fraction][±hh:mm]);
//   - BINARY, VARBINARY, LONG VARBINARY: Bytes;
//   - ARRAY, SET and ROW: String. Vertica sends them to vertica-sql-go as
//     VARCHAR holding their text form ([1,2,3], {"f0":1}), so a result
//     cannot tell them from text; the catalog type maps the same way.
//
// A text or binary value longer than maxValueBytes is cut (text at a UTF-8
// boundary) and the result carries cutWarning. LONG VARCHAR and LONG
// VARBINARY hold up to 32 MB.

// maxValueBytes caps one text or binary value: 1 MiB, as the other
// drivers cap LOBs.
const maxValueBytes = 1 << 20

// cutWarning is the Stats warning of a result with a cut value.
const cutWarning = "some values were longer than 1 MiB and were cut; select a substring (SUBSTR) to see the rest"

// baseType upper-cases a type name and drops its parameters:
// "numeric(10,2)" → "NUMERIC", "Array[int8]" → "ARRAY".
func baseType(native string) string {
	t := strings.ToUpper(strings.TrimSpace(native))
	if i := strings.IndexAny(t, "(["); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

// columnType normalises a Vertica type name: a result column's type
// (vertica-sql-go's names: INT, VARCHAR, TIMESTAMPTZ …) or a catalog
// data_type (int, varchar(80), numeric(10,2) …).
func columnType(native string) driver.Type {
	t := baseType(native)
	switch {
	case strings.HasPrefix(t, "INTERVAL"):
		return driver.TypeString
	case strings.HasPrefix(t, "LONG VARCHAR"):
		return driver.TypeString
	case strings.HasPrefix(t, "LONG VARBINARY"):
		return driver.TypeBytes
	}
	switch t {
	case "INT", "INTEGER", "BIGINT", "SMALLINT", "TINYINT", "INT8":
		return driver.TypeInt
	case "NUMERIC", "DECIMAL", "NUMBER", "MONEY":
		return driver.TypeDecimal
	case "FLOAT", "FLOAT8", "REAL", "DOUBLE PRECISION":
		return driver.TypeFloat
	case "BOOL", "BOOLEAN":
		return driver.TypeBool
	case "CHAR", "CHARACTER", "VARCHAR", "CHARACTER VARYING", "UUID":
		return driver.TypeString
	case "DATE", "TIMESTAMP", "TIMESTAMPTZ", "DATETIME", "SMALLDATETIME", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITHOUT TIME ZONE":
		return driver.TypeTime
	case "TIME", "TIMETZ", "TIME WITH TIME ZONE":
		return driver.TypeString
	case "BINARY", "VARBINARY", "BYTEA", "RAW":
		return driver.TypeBytes
	case "ARRAY", "SET", "ROW":
		// Results carry them as VARCHAR holding JSON-like text, so the
		// catalog's type says the same.
		return driver.TypeString
	}
	return driver.TypeUnknown
}

// convertValue turns one value vertica-sql-go scanned into a driver.Value.
// scale is the column's numeric scale (for NUMERIC). cut is set when the
// value was cut at maxValueBytes.
func convertValue(native string, scale int64, raw any, cut *bool) driver.Value {
	if raw == nil {
		return driver.Null()
	}
	typ := columnType(native)
	base := baseType(native)
	switch x := raw.(type) {
	case bool:
		return driver.Bool(x)
	case int:
		return driver.Int(int64(x))
	case int64:
		return driver.Int(x)
	case float64:
		if typ == driver.TypeDecimal {
			return driver.Decimal(strconv.FormatFloat(x, 'f', int(max(scale, 0)), 64))
		}
		return driver.Float(x)
	case string:
		return convertString(native, typ, x, cut)
	case time.Time:
		switch base {
		case "DATE":
			return driver.Time(time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, time.UTC))
		case "TIMESTAMP", "DATETIME", "SMALLDATETIME", "TIMESTAMP WITHOUT TIME ZONE":
			return driver.Time(time.Date(x.Year(), x.Month(), x.Day(), x.Hour(), x.Minute(), x.Second(), x.Nanosecond(), time.UTC))
		case "TIME":
			return driver.String(x.Format("15:04:05.999999"))
		case "TIMETZ", "TIME WITH TIME ZONE":
			return driver.String(x.Format("15:04:05.999999-07:00"))
		}
		return driver.Time(x.UTC())
	case []byte:
		if len(x) > maxValueBytes {
			*cut = true
			x = x[:maxValueBytes]
		}
		return driver.Bytes(x)
	}
	s := fmt.Sprint(raw)
	return driver.Unknown(native, []byte(s), s)
}

// convertString converts a value vertica-sql-go hands over as text.
func convertString(native string, typ driver.Type, s string, cut *bool) driver.Value {
	if len(s) > maxValueBytes {
		*cut = true
		s = cutUTF8(s, maxValueBytes)
		if typ == driver.TypeJSON {
			return driver.String(s)
		}
	}
	switch typ {
	case driver.TypeDecimal:
		return driver.Decimal(s)
	case driver.TypeJSON:
		if json.Valid([]byte(s)) {
			return driver.JSON([]byte(s))
		}
		return driver.String(s)
	case driver.TypeString:
		return driver.String(s)
	}
	return driver.Unknown(native, []byte(s), s)
}

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

// bindValue converts a parameter to what vertica-sql-go binds. Decimals and
// JSON travel as text, which the server casts to the column's type; a time
// travels as RFC 3339 text in UTC, so its instant survives whatever the
// session's time zone is.
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
