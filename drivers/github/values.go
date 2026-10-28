package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// objectOf decodes raw as a JSON object, keeping the members raw.
func objectOf(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("the server answer is not a JSON object: %w", err)
	}
	if m == nil {
		return nil, errors.New("the server answer is null")
	}
	return m, nil
}

// member follows a dotted path ("commit.author.date") through nested
// objects. A missing member, or a null on the way, is (nil, false).
func member(obj map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	cur := obj
	parts := strings.Split(path, ".")
	for i, p := range parts {
		raw, ok := cur[p]
		if !ok || isNull(raw) {
			return nil, false
		}
		if i == len(parts)-1 {
			return raw, true
		}
		next, err := objectOf(raw)
		if err != nil {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

func isNull(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null"
}

// typedValue converts a raw JSON value to the declared column type. A
// value of another shape is kept as JSON, so a surprise in the answer is
// shown rather than lost.
func typedValue(raw json.RawMessage, typ driver.Type) driver.Value {
	if isNull(raw) {
		return driver.Null()
	}
	switch typ {
	case driver.TypeString:
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return driver.String(s)
		}
	case driver.TypeInt, driver.TypeFloat:
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			return numberValue(n)
		}
	case driver.TypeBool:
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return driver.Bool(b)
		}
	case driver.TypeTime:
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return driver.Time(t.UTC())
			}
			return driver.String(s)
		}
	case driver.TypeList:
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			out := make([]driver.Value, len(items))
			for i, it := range items {
				out[i] = anyValue(it)
			}
			return driver.List(out...)
		}
	}
	return driver.JSON(raw)
}

// anyValue converts a raw JSON value without a declared type: booleans,
// numbers (Int when whole, Float otherwise, Decimal past int64), strings and
// null to their tobira values, arrays and objects to JSON.
func anyValue(raw json.RawMessage) driver.Value {
	t := strings.TrimSpace(string(raw))
	switch {
	case t == "" || t == "null":
		return driver.Null()
	case t == "true":
		return driver.Bool(true)
	case t == "false":
		return driver.Bool(false)
	case t[0] == '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return driver.String(s)
		}
	case t[0] == '-' || t[0] >= '0' && t[0] <= '9':
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			return numberValue(n)
		}
	}
	return driver.JSON(raw)
}

// numberValue reads a JSON number: Int when whole and in range, Decimal
// when whole but too large, Float otherwise.
func numberValue(n json.Number) driver.Value {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return driver.Int(i)
		}
		return driver.Decimal(s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return driver.Decimal(s)
	}
	return driver.Float(f)
}

// toJSON converts a tobira value to what encoding/json sends for a request
// body. Bytes, vectors and unknown values have no GitHub spelling.
func toJSON(v driver.Value) (any, error) {
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
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, errors.New("NaN and infinities have no JSON spelling")
		}
		return f, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		return json.Number(s), nil
	case driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format(time.RFC3339), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		if !json.Valid(raw) {
			return nil, errors.New("invalid JSON")
		}
		return json.RawMessage(raw), nil
	case driver.TypeList:
		items, _ := v.AsList()
		out := make([]any, len(items))
		for i, it := range items {
			x, err := toJSON(it)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case driver.TypeObject:
		obj, _ := v.AsObject()
		out := make(map[string]any, len(obj))
		for _, m := range obj {
			x, err := toJSON(m.Value)
			if err != nil {
				return nil, err
			}
			out[m.Key] = x
		}
		return out, nil
	}
	return nil, fmt.Errorf("a %s value has no GitHub spelling", v.Type())
}

// stringList reads a value that GitHub takes as a list of names: a List of
// strings, a JSON array of strings, or one string (a comma-separated list).
func stringList(v driver.Value) ([]string, error) {
	switch v.Type() {
	case driver.TypeNull:
		return []string{}, nil
	case driver.TypeString:
		s, _ := v.AsText()
		if strings.TrimSpace(s) == "" {
			return []string{}, nil
		}
		parts := strings.Split(s, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts, nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		var out []string
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, errors.New("expected a JSON array of strings")
		}
		return out, nil
	case driver.TypeList:
		items, _ := v.AsList()
		out := make([]string, len(items))
		for i, it := range items {
			s, ok := it.AsText()
			if !ok {
				return nil, errors.New("expected a list of strings")
			}
			out[i] = s
		}
		return out, nil
	}
	return nil, errors.New("expected a list of strings")
}

// text reads a string value, accepting numbers (as their text) too.
func text(v driver.Value) (string, bool) {
	switch v.Type() {
	case driver.TypeString, driver.TypeDecimal:
		return v.AsText()
	case driver.TypeInt:
		i, _ := v.AsInt()
		return strconv.FormatInt(i, 10), true
	}
	return "", false
}
