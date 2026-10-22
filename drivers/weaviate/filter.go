package weaviate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// buildWhere translates a structured filter into a GraphQL where literal.
// Conditions are ANDed; Filter.Raw is a Weaviate where filter in JSON (as
// the request format's "where") ANDed with them. none reports a filter
// that matches nothing (an empty "in").
func buildWhere(f driver.Filter, s *collSchema) (where string, none bool, err error) {
	var clauses []string
	if f.Raw != "" {
		raw, err := renderLiteral([]byte(f.Raw))
		if err != nil || !strings.HasPrefix(raw, "{") {
			if err == nil {
				err = errors.New("must be an object")
			}
			return "", false, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("raw filter: %w", err))
		}
		clauses = append(clauses, raw)
	}
	for _, c := range f.Conditions {
		clause, empty, err := condition(c, s)
		if err != nil {
			return "", false, err
		}
		if empty {
			return "", true, nil
		}
		clauses = append(clauses, clause)
	}
	switch len(clauses) {
	case 0:
		return "", false, nil
	case 1:
		return clauses[0], false, nil
	}
	return "{operator: And, operands: [" + strings.Join(clauses, ", ") + "]}", false, nil
}

var compareOps = map[driver.Op]string{
	driver.OpEq: "Equal", driver.OpNe: "NotEqual",
	driver.OpLt: "LessThan", driver.OpLe: "LessThanEqual",
	driver.OpGt: "GreaterThan", driver.OpGe: "GreaterThanEqual",
}

func clause(path, operator, valueKey, value string) string {
	p, _ := json.Marshal(path)
	return fmt.Sprintf("{path: [%s], operator: %s, %s: %s}", p, operator, valueKey, value)
}

func orOf(clauses []string) string {
	if len(clauses) == 1 {
		return clauses[0]
	}
	return "{operator: Or, operands: [" + strings.Join(clauses, ", ") + "]}"
}

// condition translates one condition.
func condition(c driver.Condition, s *collSchema) (string, bool, error) {
	unsupported := func(format string, args ...any) (string, bool, error) {
		return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf(format, args...))
	}
	if c.Field == idColumn {
		return idCondition(c)
	}
	if _, ok := s.vectorByColumn(c.Field); ok {
		return unsupported("cannot filter on vector %q", c.Field)
	}
	p, ok := s.prop(c.Field)
	if !ok {
		return "", false, driver.Wrap(driver.ErrNotFound, Name, "browse", fmt.Errorf("collection %s has no property %q", s.class, c.Field))
	}
	t, arr := baseType(p.dataType)
	valueKey, ok := valueKeys[t]
	if !ok || isReference(p.dataType) {
		return unsupported("cannot filter on %s property %q", nativeType(p.dataType), c.Field)
	}
	lit := func(v driver.Value) (string, error) { return literal(t, c.Field, v) }
	switch c.Op {
	case driver.OpIsNull, driver.OpNotNull:
		return clause(c.Field, "IsNull", "valueBoolean", strconv.FormatBool(c.Op == driver.OpIsNull)), false, nil
	case driver.OpIn:
		items, ok := c.Value.AsList()
		if !ok {
			return "", false, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("in needs a list, got %s", c.Value.Type()))
		}
		if len(items) == 0 {
			return "", true, nil
		}
		lits := make([]string, len(items))
		for i, it := range items {
			l, err := lit(it)
			if err != nil {
				return "", false, err
			}
			lits[i] = l
		}
		if arr {
			return clause(c.Field, "ContainsAny", valueKey, "["+strings.Join(lits, ", ")+"]"), false, nil
		}
		ors := make([]string, len(lits))
		for i, l := range lits {
			ors[i] = clause(c.Field, "Equal", valueKey, l)
		}
		return orOf(ors), false, nil
	}
	if arr {
		// An array matches when it holds the value.
		if c.Op != driver.OpEq && c.Op != driver.OpContains {
			return unsupported("%s is not supported on list property %q: use eq, contains or in", c.Op, c.Field)
		}
		l, err := lit(c.Value)
		if err != nil {
			return "", false, err
		}
		return clause(c.Field, "ContainsAny", valueKey, "["+l+"]"), false, nil
	}
	switch c.Op {
	case driver.OpContains, driver.OpPrefix:
		if valueKey != "valueText" || t == "uuid" {
			return unsupported("%s is supported on text properties only", c.Op)
		}
		v, ok := c.Value.AsText()
		if !ok || c.Value.Type() != driver.TypeString {
			return "", false, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("%s needs a string", c.Op))
		}
		if !utf8.ValidString(v) {
			return "", false, errInvalidUTF8("browse", c.Field)
		}
		if strings.ContainsAny(v, "*?") {
			return unsupported("%s cannot match * or ?: Weaviate's Like has no escape", c.Op)
		}
		pattern := v + "*"
		if c.Op == driver.OpContains {
			pattern = "*" + v + "*"
		}
		q, _ := json.Marshal(pattern)
		return clause(c.Field, "Like", "valueText", string(q)), false, nil
	}
	operator, ok := compareOps[c.Op]
	if !ok {
		return unsupported("operator %q is not supported", c.Op)
	}
	l, err := lit(c.Value)
	if err != nil {
		return "", false, err
	}
	return clause(c.Field, operator, valueKey, l), false, nil
}

// valueKeys maps an element type to the where filter's value argument.
var valueKeys = map[string]string{
	"text": "valueText", "string": "valueText", "uuid": "valueText",
	"int": "valueInt", "number": "valueNumber", "boolean": "valueBoolean", "date": "valueDate",
}

// literal writes a filter value of element type t as a GraphQL literal.
func literal(t, field string, v driver.Value) (string, error) {
	bad := func() (string, error) {
		return "", driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("property %q is %s: cannot compare it with a %s", field, t, v.Type()))
	}
	switch t {
	case "text", "string", "uuid":
		if v.Type() == driver.TypeString {
			s, _ := v.AsText()
			if !utf8.ValidString(s) {
				return "", errInvalidUTF8("browse", field)
			}
			q, _ := json.Marshal(s)
			return string(q), nil
		}
	case "int":
		switch v.Type() {
		case driver.TypeInt:
			n, _ := v.AsInt()
			return strconv.FormatInt(n, 10), nil
		case driver.TypeFloat:
			f, _ := v.AsFloat()
			if f == math.Trunc(f) && math.Abs(f) < 1<<63 {
				return strconv.FormatInt(int64(f), 10), nil
			}
		case driver.TypeDecimal:
			n, err := decimalInt("browse", field, v)
			if err != nil {
				return "", err
			}
			return strconv.FormatInt(n, 10), nil
		}
	case "number":
		switch v.Type() {
		case driver.TypeInt:
			n, _ := v.AsInt()
			return strconv.FormatInt(n, 10), nil
		case driver.TypeFloat:
			f, _ := v.AsFloat()
			if !math.IsNaN(f) && !math.IsInf(f, 0) {
				return strconv.FormatFloat(f, 'g', -1, 64), nil
			}
		case driver.TypeDecimal:
			s, _ := v.AsText()
			if _, err := strconv.ParseFloat(s, 64); err == nil && json.Valid([]byte(s)) {
				return s, nil
			}
		}
	case "boolean":
		if b, ok := v.AsBool(); ok {
			return strconv.FormatBool(b), nil
		}
	case "date":
		var tm time.Time
		switch v.Type() {
		case driver.TypeTime:
			tm, _ = v.AsTime()
		case driver.TypeString:
			s, _ := v.AsText()
			var err error
			if tm, err = time.Parse(time.RFC3339Nano, s); err != nil {
				return bad()
			}
		default:
			return bad()
		}
		q, _ := json.Marshal(tm.Format(time.RFC3339Nano))
		return string(q), nil
	}
	return bad()
}

// idCondition filters on the object id: eq, ne and in.
func idCondition(c driver.Condition) (string, bool, error) {
	uuidLit := func(v driver.Value) (string, error) {
		u, err := toUUID(v, "browse")
		if err != nil {
			return "", err
		}
		return strconv.Quote(u), nil
	}
	switch c.Op {
	case driver.OpEq, driver.OpNe:
		l, err := uuidLit(c.Value)
		if err != nil {
			return "", false, err
		}
		return clause(idColumn, compareOps[c.Op], "valueText", l), false, nil
	case driver.OpIn:
		items, ok := c.Value.AsList()
		if !ok {
			return "", false, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("in needs a list, got %s", c.Value.Type()))
		}
		if len(items) == 0 {
			return "", true, nil
		}
		ors := make([]string, len(items))
		for i, it := range items {
			l, err := uuidLit(it)
			if err != nil {
				return "", false, err
			}
			ors[i] = clause(idColumn, "Equal", "valueText", l)
		}
		return orOf(ors), false, nil
	}
	return "", false, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("the id is a UUID: filter it with eq, ne or in, not %s", c.Op))
}
