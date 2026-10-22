package redis

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// Redis stores hash fields as strings. When hashes are shown as records
// (Describe, Browse of a hash collection, FT.SEARCH results), each field
// gets the narrowest type that every sampled value of it parses as, and the
// text is shown as that type. Only canonical spellings count, so the text
// round-trips: "7" is an int but "007" and "+7" stay strings.
//
//	int      canonical base-10 int64 ("-12")
//	decimal  digits, a point and digits ("12.50"); kept exactly as written
//	bool     "true" or "false"
//	time     RFC 3339 ("2026-01-02T03:04:05Z")
//	json     a JSON object or array
//	bytes    not valid UTF-8
//	string   anything else

// maxDecimalLen bounds the text inferred as a decimal.
const maxDecimalLen = 40

// inferType returns the type a single value parses as.
func inferType(s string) driver.Type {
	switch {
	case !utf8.ValidString(s):
		return driver.TypeBytes
	case s == "true" || s == "false":
		return driver.TypeBool
	case isCanonicalInt(s):
		return driver.TypeInt
	case isDecimal(s):
		return driver.TypeDecimal
	case isTime(s):
		return driver.TypeTime
	case isJSONContainer(s):
		return driver.TypeJSON
	}
	return driver.TypeString
}

func isCanonicalInt(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && strconv.FormatInt(n, 10) == s
}

// isDecimal matches -?(0|[1-9][0-9]*)\.[0-9]+ .
func isDecimal(s string) bool {
	if len(s) > maxDecimalLen {
		return false
	}
	s = strings.TrimPrefix(s, "-")
	whole, frac, ok := strings.Cut(s, ".")
	if !ok || whole == "" || frac == "" || (len(whole) > 1 && whole[0] == '0') {
		return false
	}
	return allDigits(whole) && allDigits(frac)
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isTime(s string) bool {
	if len(s) < len("2006-01-02T15:04:05Z") || s[10] != 'T' {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

func isJSONContainer(s string) bool {
	t := strings.TrimSpace(s)
	return t != "" && (t[0] == '{' || t[0] == '[') && json.Valid([]byte(t))
}

// unifyTypes combines the types of two values of one field. TypeNull (no
// value yet) is the identity, int and decimal widen to decimal, bytes wins
// over the text types, and any other mix is string.
func unifyTypes(a, b driver.Type) driver.Type {
	switch {
	case a == driver.TypeNull:
		return b
	case b == driver.TypeNull, a == b:
		return a
	case a == driver.TypeBytes || b == driver.TypeBytes:
		return driver.TypeBytes
	case (a == driver.TypeInt && b == driver.TypeDecimal) || (a == driver.TypeDecimal && b == driver.TypeInt):
		return driver.TypeDecimal
	}
	return driver.TypeString
}

// typedValue shows s as t. A value that does not parse as t (one written
// after the sample was taken) is shown as text, so nothing is lost.
func typedValue(s string, t driver.Type) driver.Value {
	switch t {
	case driver.TypeInt:
		if isCanonicalInt(s) {
			n, _ := strconv.ParseInt(s, 10, 64)
			return driver.Int(n)
		}
	case driver.TypeDecimal:
		if isCanonicalInt(s) || isDecimal(s) {
			return driver.Decimal(s)
		}
	case driver.TypeBool:
		if s == "true" || s == "false" {
			return driver.Bool(s == "true")
		}
	case driver.TypeTime:
		if isTime(s) {
			ts, _ := time.Parse(time.RFC3339Nano, s)
			return driver.Time(ts)
		}
	case driver.TypeJSON:
		if isJSONContainer(s) {
			return driver.JSON([]byte(s))
		}
	}
	return textValue(s)
}

// textValue is s as a String, or Bytes when it is not valid UTF-8.
func textValue(s string) driver.Value {
	if !utf8.ValidString(s) {
		return driver.Bytes([]byte(s))
	}
	return driver.String(s)
}

// inferFields types each named field over the sampled records. Every field
// is nullable, since any hash may lack it.
func inferFields(names []string, recs []map[string]string) []driver.Field {
	fields := make([]driver.Field, 0, len(names))
	for _, name := range names {
		t := driver.TypeNull
		for _, r := range recs {
			if v, ok := r[name]; ok {
				t = unifyTypes(t, inferType(v))
			}
		}
		if t == driver.TypeNull {
			t = driver.TypeString
		}
		fields = append(fields, driver.Field{Name: name, NativeType: "hash field", Type: t, Nullable: true})
	}
	return fields
}

// encodeValue is the text the driver writes for v: the inverse of
// typedValue for the inferred types. Lists and objects are written as JSON.
// NULL has no text; callers delete the field or refuse instead.
func encodeValue(v driver.Value) (string, error) {
	switch v.Type() {
	case driver.TypeNull:
		return "", errors.New("NULL has no Redis text")
	case driver.TypeString, driver.TypeDecimal:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		return strconv.FormatInt(n, 10), nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return strconv.FormatBool(b), nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format(time.RFC3339Nano), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return string(raw), nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return string(b), nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
