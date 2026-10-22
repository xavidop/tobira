package pinecone

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xavidop/tobira/driver"
)

// Pinecone lists records without filtering or ordering them, so Browse
// filters and sorts on the client (matchAll, sortRecords) while it scans.
// A vector search filters on the server (pineconeFilter).

// fieldValue is the value of column field in r: the record id for _id,
// NULL when the metadata has no such key.
func fieldValue(r record, field string) driver.Value {
	if field == idColumn {
		return driver.String(r.id)
	}
	if v, ok := r.meta[metaKey(field)]; ok {
		return v
	}
	return driver.Null()
}

// checkConditions refuses what matchAll cannot evaluate, before any scan.
func checkConditions(conds []driver.Condition, op string) error {
	for _, c := range conds {
		switch c.Op {
		case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe,
			driver.OpContains, driver.OpPrefix, driver.OpIsNull, driver.OpNotNull:
		case driver.OpIn:
			if _, ok := c.Value.AsList(); !ok {
				return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s in needs a list", c.Field))
			}
		default:
			return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("unknown filter operator %q", c.Op))
		}
		if c.Field == valuesColumn || c.Field == sparseColumn {
			return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("cannot filter on %s", c.Field))
		}
	}
	return nil
}

// matchAll reports whether r satisfies every condition. A missing key is
// NULL: it fails every comparison but ne and is_null. contains is a
// substring test on text and a member test on a list of strings; prefix
// applies to text. Anything else is ErrUnsupported.
func matchAll(conds []driver.Condition, r record) (bool, error) {
	if err := checkConditions(conds, "browse"); err != nil {
		return false, err
	}
	for _, c := range conds {
		ok, err := match(c, fieldValue(r, c.Field))
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func match(c driver.Condition, v driver.Value) (bool, error) {
	unsupported := func(format string, args ...any) (bool, error) {
		return false, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf(format, args...))
	}
	switch c.Op {
	case driver.OpIsNull:
		return v.IsNull(), nil
	case driver.OpNotNull:
		return !v.IsNull(), nil
	}
	if v.IsNull() {
		return c.Op == driver.OpNe, nil
	}
	switch c.Op {
	case driver.OpEq:
		return equalValue(v, c.Value), nil
	case driver.OpNe:
		return !equalValue(v, c.Value), nil
	case driver.OpIn:
		items, _ := c.Value.AsList()
		return slices.ContainsFunc(items, func(it driver.Value) bool { return equalValue(v, it) }), nil
	case driver.OpContains:
		want, ok := c.Value.AsText()
		if !ok {
			return unsupported("contains needs a text value")
		}
		if s, isText := v.AsText(); isText && v.Type() == driver.TypeString {
			return strings.Contains(s, want), nil
		}
		if items, isList := v.AsList(); isList {
			return slices.ContainsFunc(items, func(it driver.Value) bool { s, _ := it.AsText(); return s == want }), nil
		}
		return unsupported("contains applies to text and lists, not to %s %q", v.Type(), c.Field)
	case driver.OpPrefix:
		want, ok := c.Value.AsText()
		s, isText := v.AsText()
		if !ok || !isText || v.Type() != driver.TypeString {
			return unsupported("prefix applies to text, not to %s %q", v.Type(), c.Field)
		}
		return strings.HasPrefix(s, want), nil
	}
	// lt, le, gt, ge
	if kindRank(v) == rankOther || kindRank(v) != kindRank(c.Value) {
		if kindRank(v) == rankOther {
			return unsupported("cannot order %s %q", v.Type(), c.Field)
		}
		return false, nil
	}
	n := compareSort(v, c.Value)
	switch c.Op {
	case driver.OpLt:
		return n < 0, nil
	case driver.OpLe:
		return n <= 0, nil
	case driver.OpGt:
		return n > 0, nil
	}
	return n >= 0, nil
}

// equalValue compares a stored value with a filter value: numbers by
// value, whatever their types, everything else exactly.
func equalValue(stored, want driver.Value) bool {
	if a, ok := numeric(stored); ok {
		b, ok := numeric(want)
		return ok && a.Cmp(b) == 0
	}
	if stored.Type() != want.Type() {
		return false
	}
	switch stored.Type() {
	case driver.TypeString:
		a, _ := stored.AsText()
		b, _ := want.AsText()
		return a == b
	case driver.TypeBool:
		a, _ := stored.AsBool()
		b, _ := want.AsBool()
		return a == b
	}
	return stored.String() == want.String()
}

// numeric is v as an exact number, for an Int, a finite Float or a Decimal.
func numeric(v driver.Value) (*big.Rat, bool) {
	switch v.Type() {
	case driver.TypeInt:
		n, _ := v.AsInt()
		return new(big.Rat).SetInt64(n), true
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		r := new(big.Rat)
		if r.SetFloat64(f) == nil {
			return nil, false
		}
		return r, true
	case driver.TypeDecimal:
		s, _ := v.AsText()
		return new(big.Rat).SetString(s)
	}
	return nil, false
}

// The kinds compareSort orders, in order.
const (
	rankBool = iota
	rankNumber
	rankString
	rankOther
)

func kindRank(v driver.Value) int {
	switch v.Type() {
	case driver.TypeBool:
		return rankBool
	case driver.TypeInt, driver.TypeFloat, driver.TypeDecimal:
		return rankNumber
	case driver.TypeString:
		return rankString
	}
	return rankOther
}

// compareSort orders two non-NULL values: booleans, then numbers, then
// strings, then anything else by its text.
func compareSort(a, b driver.Value) int {
	ra, rb := kindRank(a), kindRank(b)
	if ra != rb {
		return cmp.Compare(ra, rb)
	}
	switch ra {
	case rankBool:
		x, _ := a.AsBool()
		y, _ := b.AsBool()
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		}
		return 1
	case rankNumber:
		x, okx := numeric(a)
		y, oky := numeric(b)
		if okx && oky {
			return x.Cmp(y)
		}
	case rankString:
		x, _ := a.AsText()
		y, _ := b.AsText()
		return strings.Compare(x, y)
	}
	return strings.Compare(a.String(), b.String())
}

// sortRecords orders recs by field, records without it last in either
// direction, ties by id.
func sortRecords(recs []record, field string, desc bool) {
	slices.SortStableFunc(recs, func(a, b record) int {
		va, vb := fieldValue(a, field), fieldValue(b, field)
		switch {
		case va.IsNull() && vb.IsNull():
			return strings.Compare(a.id, b.id)
		case va.IsNull():
			return 1
		case vb.IsNull():
			return -1
		}
		n := compareSort(va, vb)
		if desc {
			n = -n
		}
		if n == 0 {
			return strings.Compare(a.id, b.id)
		}
		return n
	})
}

// pineconeFilter translates a filter for a server-side metadata filter: the
// conditions are ANDed, or Raw is a Pinecone filter object as is. NULL
// tests become $exists. contains, prefix and conditions on _id have no
// Pinecone equivalent and are ErrUnsupported.
func pineconeFilter(f driver.Filter) (*structpb.Struct, error) {
	unsupported := func(format string, args ...any) (*structpb.Struct, error) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf(format, args...))
	}
	if strings.TrimSpace(f.Raw) != "" {
		if len(f.Conditions) > 0 {
			return unsupported("give a raw filter or conditions, not both")
		}
		return rawFilter(f.Raw)
	}
	if len(f.Conditions) == 0 {
		return nil, nil
	}
	ops := map[driver.Op]string{driver.OpEq: "$eq", driver.OpNe: "$ne", driver.OpLt: "$lt", driver.OpLe: "$lte", driver.OpGt: "$gt", driver.OpGe: "$gte", driver.OpIn: "$in"}
	var parts []any
	for _, c := range f.Conditions {
		if c.Field == idColumn || c.Field == valuesColumn || c.Field == sparseColumn || c.Field == "" {
			return unsupported("a vector search filters on metadata only, not on %q", c.Field)
		}
		var cond any
		switch c.Op {
		case driver.OpIsNull:
			cond = map[string]any{"$exists": false}
		case driver.OpNotNull:
			cond = map[string]any{"$exists": true}
		case driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
			x, err := filterValue(c.Value)
			if _, isNum := x.(float64); err != nil || !isNum {
				return unsupported("%s %s needs a number", c.Field, c.Op)
			}
			cond = map[string]any{ops[c.Op]: x}
		case driver.OpEq, driver.OpNe, driver.OpIn:
			var x any
			var err error
			if c.Op == driver.OpIn {
				items, ok := c.Value.AsList()
				if !ok {
					return unsupported("%s in needs a list", c.Field)
				}
				list := make([]any, len(items))
				for i, it := range items {
					if list[i], err = filterValue(it); err != nil {
						return nil, err
					}
				}
				x = list
			} else if x, err = filterValue(c.Value); err != nil {
				return nil, err
			}
			cond = map[string]any{ops[c.Op]: x}
		default:
			return unsupported("Pinecone metadata filters have no %q operator", c.Op)
		}
		parts = append(parts, map[string]any{metaKey(c.Field): cond})
	}
	s, err := structpb.NewStruct(map[string]any{"$and": parts})
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", err)
	}
	return s, nil
}

// filterValue converts a condition value for a Pinecone filter.
func filterValue(v driver.Value) (any, error) {
	pv, err := toMeta("filter", v)
	if err != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "similar", errors.New("a filter value must be a string, number or boolean"))
	}
	if pv == nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "similar", errors.New("compare with NULL using is_null or not_null"))
	}
	if _, isList := pv.GetKind().(*structpb.Value_ListValue); isList {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "similar", errors.New("a filter value must be a string, number or boolean"))
	}
	return pv.AsInterface(), nil
}

// rawFilter parses exactly one JSON object.
func rawFilter(raw string) (*structpb.Struct, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", errors.New("a raw filter must be a Pinecone filter object"))
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", errors.New("a raw filter must be one JSON object"))
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", err)
	}
	return s, nil
}
