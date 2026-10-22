package elasticsearch

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// kv is one member of a JSON object, in document order.
type kv struct {
	key string
	raw json.RawMessage
}

// orderedObject splits a JSON object into its members in document order.
func orderedObject(data []byte) ([]kv, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var out []kv
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, kv{k, raw})
	}
	return out, nil
}

// convertValue turns one source value into a tobira Value as its mapping
// says. An array holds values of the field's type (every field may hold
// several), and a value that does not fit the mapping (ES coerces "7" into
// an integer field, for example) is read as well as it can be: a numeric
// string as a number, otherwise its JSON shape. A nil field (unmapped)
// converts by JSON shape.
func convertValue(raw json.RawMessage, f *field) driver.Value {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return driver.Null()
	}
	if f == nil {
		return genericValue(raw)
	}
	switch fieldType(f) {
	case driver.TypeJSON:
		return compactJSON(raw)
	case driver.TypeVector:
		var v []float32
		if json.Unmarshal(raw, &v) == nil {
			return driver.Vector(v)
		}
		return genericValue(raw)
	case driver.TypeList: // nested
		if raw[0] == '{' {
			return driver.List(objectValue(raw, f.props))
		}
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return genericValue(raw)
		}
		out := make([]driver.Value, len(items))
		for i, it := range items {
			out[i] = objectValue(it, f.props)
		}
		return driver.List(out...)
	}
	if raw[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return genericValue(raw)
		}
		out := make([]driver.Value, len(items))
		for i, it := range items {
			out[i] = convertValue(it, f)
		}
		return driver.List(out...)
	}
	switch fieldType(f) {
	case driver.TypeObject:
		if raw[0] == '{' {
			return objectValue(raw, f.props)
		}
	case driver.TypeInt:
		if s, ok := numberText(raw); ok {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return driver.Int(n)
			}
			return decimalOrFloat(s)
		}
	case driver.TypeFloat:
		if s, ok := numberText(raw); ok {
			if x, err := strconv.ParseFloat(s, 64); err == nil {
				return driver.Float(x)
			}
		}
	case driver.TypeDecimal:
		if s, ok := numberText(raw); ok {
			return driver.Decimal(s)
		}
	case driver.TypeBool:
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return driver.Bool(b)
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && (s == "true" || s == "false") {
			return driver.Bool(s == "true")
		}
	case driver.TypeTime:
		if t, ok := parseTime(raw); ok {
			return driver.Time(t)
		}
	case driver.TypeBytes:
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return driver.Bytes(b)
			}
		}
	case driver.TypeString:
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return driver.String(s)
		}
		if raw[0] != '{' {
			return driver.String(string(raw)) // a number or bool in a text field
		}
	}
	return genericValue(raw)
}

// objectValue converts a JSON object with its properties' mapping.
func objectValue(raw json.RawMessage, props *mapping) driver.Value {
	kvs, err := orderedObject(raw)
	if err != nil {
		return genericValue(raw)
	}
	out := make(driver.Object, len(kvs))
	for i, m := range kvs {
		out[i] = driver.Member{Key: m.key, Value: convertValue(m.raw, props.lookup(m.key))}
	}
	return driver.ObjectValue(out...)
}

// numberText returns the text of a JSON number, or of a string holding a
// valid JSON number.
func numberText(raw json.RawMessage) (string, bool) {
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		raw = json.RawMessage(strings.TrimSpace(s))
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil || n == "" {
		return "", false
	}
	if _, err := strconv.ParseFloat(n.String(), 64); err != nil {
		if _, ok := new(big.Rat).SetString(n.String()); !ok {
			return "", false
		}
	}
	return n.String(), true
}

// decimalOrFloat keeps an integer too large for int64 exact, as a
// Decimal, and reads anything else as a Float.
func decimalOrFloat(s string) driver.Value {
	if !strings.ContainsAny(s, ".eE") {
		return driver.Decimal(s)
	}
	x, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return driver.Decimal(s)
	}
	return driver.Float(x)
}

// timeLayouts are the date formats a date field's source value is tried
// against, after RFC 3339: ES's strict_date_optional_time forms.
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006-01",
	"2006",
}

// parseTime reads a date field's source value: a date string, or epoch
// milliseconds as a number. Times are in UTC.
func parseTime(raw json.RawMessage) (time.Time, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return time.Time{}, false
		}
		ms, err := n.Int64()
		if err != nil {
			return time.Time{}, false
		}
		return time.UnixMilli(ms).UTC(), true
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC(), true
	}
	return time.Time{}, false
}

// genericValue converts JSON by its shape: integers that fit int64 as Int
// (larger ones as Decimal), other numbers as Float, objects and arrays
// recursively.
func genericValue(raw json.RawMessage) driver.Value {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return driver.String(string(raw))
	}
	if kvs, err := orderedObject(raw); err == nil {
		out := make(driver.Object, len(kvs))
		for i, m := range kvs {
			out[i] = driver.Member{Key: m.key, Value: genericValue(m.raw)}
		}
		return driver.ObjectValue(out...)
	}
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
		return decimalOrFloat(v.String())
	case []any:
		var items []json.RawMessage
		_ = json.Unmarshal(raw, &items)
		out := make([]driver.Value, len(items))
		for i, it := range items {
			out[i] = genericValue(it)
		}
		return driver.List(out...)
	}
	return compactJSON(raw)
}

func compactJSON(raw json.RawMessage) driver.Value {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return driver.JSON(raw)
	}
	return driver.JSON(buf.Bytes())
}

// valueJSON encodes a tobira Value for a write or a query: decimals as
// their exact digits, times as RFC 3339 in UTC, bytes as base64 (the
// binary type's form), JSON as itself.
func valueJSON(v driver.Value) (json.RawMessage, error) {
	switch v.Type() {
	case driver.TypeNull:
		return json.RawMessage("null"), nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		if s == "" || s[0] == '"' {
			return nil, fmt.Errorf("decimal %q is not a JSON number", s)
		}
		if _, ok := numberText(json.RawMessage(s)); !ok {
			return nil, fmt.Errorf("decimal %q is not a JSON number", s)
		}
		return json.RawMessage(s), nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return json.Marshal(t.UTC().Format(time.RFC3339Nano))
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, fmt.Errorf("invalid JSON value: %v", err)
		}
		return buf.Bytes(), nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return json.Marshal(base64.StdEncoding.EncodeToString(b))
	case driver.TypeList:
		items, _ := v.AsList()
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, it := range items {
			if i > 0 {
				buf.WriteByte(',')
			}
			raw, err := valueJSON(it)
			if err != nil {
				return nil, err
			}
			buf.Write(raw)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	case driver.TypeObject:
		obj, _ := v.AsObject()
		return objectJSON(obj, nil)
	case driver.TypeUnknown:
		_, raw, _ := v.AsUnknown()
		if json.Valid(raw) {
			return raw, nil
		}
		return json.Marshal(v.String())
	}
	return v.MarshalJSON()
}

// objectJSON encodes an Object as a JSON object in member order, leaving
// out the members skip names.
func objectJSON(obj driver.Object, skip map[string]bool) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	n := 0
	for _, m := range obj {
		if skip[m.Key] {
			continue
		}
		raw, err := valueJSON(m.Value)
		if err != nil {
			return nil, fmt.Errorf("field %q: %v", m.Key, err)
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(m.Key)
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(raw)
		n++
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// readJSON decodes data into v, keeping numbers exact.
func readJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return err
	}
	return nil
}
