package elasticsearch

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// buildQuery translates a Filter into a query clause: the conditions and
// the raw clause ANDed in a bool filter (not-equal and is-null as
// must_not), or match_all when there are none. Text fields match through
// their keyword multi-field where they have one; contains and prefix on
// text are phrase matches. With readOnly, a raw clause holding a script is
// refused, as in Query.
func buildQuery(f driver.Filter, m *mapping, readOnly bool) (map[string]any, error) {
	return buildQueryAs(Name, "browse", f, m, readOnly, false)
}

func buildQueryAs(name, op string, f driver.Filter, m *mapping, readOnly, allowSystem bool) (map[string]any, error) {
	var filter, mustNot []any
	for _, c := range f.Conditions {
		clause, negate, err := conditionClause(name, op, c, m)
		if err != nil {
			return nil, err
		}
		if negate {
			mustNot = append(mustNot, clause)
		} else {
			filter = append(filter, clause)
		}
	}
	if raw := strings.TrimSpace(f.Raw); raw != "" {
		sc, err := scanJSONFull([]byte(raw))
		if err == nil && (raw[0] != '{') {
			err = errors.New("a raw filter is one query clause, a JSON object")
		}
		if err != nil {
			return nil, errRefused(name, readOnly, driver.ErrSyntax, "raw filter: %v", err)
		}
		if readOnly {
			if reason := sc.readOnlyReason(allowSystem); reason != "" {
				return nil, errRefused(name, readOnly, driver.ErrReadOnly, "raw filter: %s", reason)
			}
		}
		filter = append(filter, json.RawMessage(raw))
	}
	if len(filter) == 0 && len(mustNot) == 0 {
		return map[string]any{"match_all": map[string]any{}}, nil
	}
	b := map[string]any{}
	if len(filter) > 0 {
		b["filter"] = filter
	}
	if len(mustNot) > 0 {
		b["must_not"] = mustNot
	}
	return map[string]any{"bool": b}, nil
}

// conditionClause translates one condition; negate puts it in must_not.
func conditionClause(name, op string, c driver.Condition, m *mapping) (any, bool, error) {
	unsupported := func(format string, args ...any) (any, bool, error) {
		return nil, false, driver.Wrap(driver.ErrUnsupported, name, op, fmt.Errorf(format, args...))
	}
	if c.Field == "" {
		return nil, false, driver.Wrap(driver.ErrSyntax, name, op, errors.New("a condition needs a field"))
	}
	f := m.lookup(c.Field)
	exists := map[string]any{"exists": map[string]any{"field": c.Field}}
	one := func(kind string, body any) map[string]any { return map[string]any{kind: body} }
	val := func(v driver.Value) (json.RawMessage, error) {
		raw, err := valueJSON(v)
		if err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, name, op, err)
		}
		return raw, nil
	}
	// exact matches the value with a term query, or a phrase on text with
	// no keyword multi-field.
	exact := func(v driver.Value) (any, error) {
		raw, err := val(v)
		if err != nil {
			return nil, err
		}
		if p, ok := f.keywordPath(c.Field); ok || !f.isText() {
			return one("term", map[string]any{p: raw}), nil
		}
		return one("match_phrase", map[string]any{c.Field: raw}), nil
	}
	switch c.Op {
	case driver.OpIsNull:
		return exists, true, nil
	case driver.OpNotNull:
		return exists, false, nil
	case driver.OpEq, driver.OpNe:
		if c.Value.IsNull() {
			return exists, c.Op == driver.OpEq, nil
		}
		clause, err := exact(c.Value)
		return clause, c.Op == driver.OpNe, err
	case driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		raw, err := val(c.Value)
		if err != nil {
			return nil, false, err
		}
		op := map[driver.Op]string{driver.OpLt: "lt", driver.OpLe: "lte", driver.OpGt: "gt", driver.OpGe: "gte"}[c.Op]
		p, _ := f.keywordPath(c.Field)
		return one("range", map[string]any{p: map[string]any{op: raw}}), false, nil
	case driver.OpContains, driver.OpPrefix:
		s, ok := c.Value.AsText()
		if !ok || c.Value.Type() != driver.TypeString {
			return unsupported("%s needs a string value", c.Op)
		}
		if f.isText() {
			kind := map[driver.Op]string{driver.OpContains: "match_phrase", driver.OpPrefix: "match_phrase_prefix"}[c.Op]
			return one(kind, map[string]any{c.Field: s}), false, nil
		}
		p, ok := f.keywordPath(c.Field)
		if !ok && f != nil {
			return unsupported("%s on %s field %q (only text and keyword fields)", c.Op, f.typ, c.Field)
		}
		if c.Op == driver.OpPrefix {
			return one("prefix", map[string]any{p: s}), false, nil
		}
		return one("wildcard", map[string]any{p: map[string]any{"value": "*" + escapeWildcard(s) + "*"}}), false, nil
	case driver.OpIn:
		items, ok := c.Value.AsList()
		if !ok {
			return nil, false, driver.Wrap(driver.ErrSyntax, name, op, errors.New("in needs a list value"))
		}
		raws := make([]json.RawMessage, len(items))
		for i, it := range items {
			raw, err := val(it)
			if err != nil {
				return nil, false, err
			}
			raws[i] = raw
		}
		if p, ok := f.keywordPath(c.Field); ok || !f.isText() {
			return one("terms", map[string]any{p: raws}), false, nil
		}
		should := make([]any, len(raws))
		for i, raw := range raws {
			should[i] = one("match_phrase", map[string]any{c.Field: raw})
		}
		return one("bool", map[string]any{"should": should, "minimum_should_match": 1}), false, nil
	}
	return unsupported("operator %q", c.Op)
}

// escapeWildcard escapes the wildcard query's special characters.
func escapeWildcard(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`).Replace(s)
}

// buildSort translates sort keys, sorting text fields by their keyword
// multi-field. Fields that cannot be sorted (text without one, disabled
// objects, vectors) are ErrUnsupported.
func buildSort(keys []driver.SortKey, m *mapping) ([]any, error) {
	return buildSortAs(Name, keys, m)
}

func buildSortAs(name string, keys []driver.SortKey, m *mapping) ([]any, error) {
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		f := m.lookup(k.Field)
		p := k.Field
		if f != nil {
			var ok bool
			p, ok = f.keywordPath(k.Field)
			switch t := fieldType(f); {
			case f.isText() && !ok:
				return nil, driver.Wrap(driver.ErrUnsupported, name, "browse", fmt.Errorf("text field %q has no keyword multi-field to sort on", k.Field))
			case t == driver.TypeJSON || t == driver.TypeObject || t == driver.TypeList || t == driver.TypeVector || t == driver.TypeBytes:
				return nil, driver.Wrap(driver.ErrUnsupported, name, "browse", fmt.Errorf("cannot sort on %s field %q", nativeType(f), k.Field))
			}
		}
		order := "asc"
		if k.Desc {
			order = "desc"
		}
		out = append(out, map[string]any{p: map[string]any{"order": order}})
	}
	return out, nil
}

// browseCursor is a Browse position: the point in time the browse reads
// and the sort values of the last row shown.
type browseCursor struct {
	PIT   string          `json:"p"`
	After json.RawMessage `json:"a"`
}

func (c browseCursor) encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(s string) (browseCursor, error) {
	return decodeCursorAs(Name, s)
}

func decodeCursorAs(name, s string) (browseCursor, error) {
	bad := driver.Wrap(driver.ErrSyntax, name, "browse", errors.New("invalid cursor"))
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return browseCursor{}, bad
	}
	var c browseCursor
	if err := json.Unmarshal(b, &c); err != nil || c.PIT == "" || len(c.After) == 0 || bytes.TrimSpace(c.After)[0] != '[' {
		return browseCursor{}, bad
	}
	return c, nil
}
