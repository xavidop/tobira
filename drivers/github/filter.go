package github

import (
	"fmt"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// matches applies one Browse condition to a column value in tobira, for
// the conditions the list endpoint cannot apply itself.
func matches(v driver.Value, cond driver.Condition) (bool, error) {
	switch cond.Op {
	case driver.OpIsNull:
		return v.IsNull(), nil
	case driver.OpNotNull:
		return !v.IsNull(), nil
	}
	if v.IsNull() {
		return false, nil
	}
	switch cond.Op {
	case driver.OpIn:
		items, ok := cond.Value.AsList()
		if !ok {
			return false, fmt.Errorf("%q: in takes a list", cond.Field)
		}
		for _, it := range items {
			if c, ok := compareValues(v, it); ok && c == 0 {
				return true, nil
			}
		}
		return false, nil
	case driver.OpContains:
		if items, ok := v.AsList(); ok {
			for _, it := range items {
				if c, ok := compareValues(it, cond.Value); ok && c == 0 {
					return true, nil
				}
			}
			return false, nil
		}
		s, ok1 := text(v)
		needle, ok2 := text(cond.Value)
		return ok1 && ok2 && strings.Contains(s, needle), nil
	case driver.OpPrefix:
		s, ok1 := text(v)
		prefix, ok2 := text(cond.Value)
		return ok1 && ok2 && strings.HasPrefix(s, prefix), nil
	}
	c, ok := compareValues(v, cond.Value)
	if !ok {
		return false, nil
	}
	switch cond.Op {
	case driver.OpEq:
		return c == 0, nil
	case driver.OpNe:
		return c != 0, nil
	case driver.OpLt:
		return c < 0, nil
	case driver.OpLe:
		return c <= 0, nil
	case driver.OpGt:
		return c > 0, nil
	case driver.OpGe:
		return c >= 0, nil
	}
	return false, fmt.Errorf("%q: operator %q is not supported", cond.Field, cond.Op)
}

// compareValues orders two values of compatible types: numbers by value,
// times by instant (a time against RFC 3339 text too), booleans, and text
// otherwise. ok is false when they cannot be compared.
func compareValues(a, b driver.Value) (int, bool) {
	if a.IsNull() || b.IsNull() {
		return 0, false
	}
	if fa, ok := a.AsFloat(); ok {
		if fb, ok := b.AsFloat(); ok {
			switch {
			case fa < fb:
				return -1, true
			case fa > fb:
				return 1, true
			}
			return 0, true
		}
	}
	if ta, ok := timeOf(a); ok {
		if tb, ok := timeOf(b); ok {
			return ta.Compare(tb), true
		}
	}
	if ba, ok := a.AsBool(); ok {
		if bb, ok := b.AsBool(); ok {
			switch {
			case ba == bb:
				return 0, true
			case !ba:
				return -1, true
			}
			return 1, true
		}
		if s, ok := text(b); ok {
			return strings.Compare(fmt.Sprint(ba), s), true
		}
		return 0, false
	}
	sa, ok1 := text(a)
	sb, ok2 := text(b)
	if ok1 && ok2 {
		return strings.Compare(sa, sb), true
	}
	return 0, false
}

// timeOf reads a time, or RFC 3339 text, as a time.
func timeOf(v driver.Value) (time.Time, bool) {
	if t, ok := v.AsTime(); ok {
		return t, true
	}
	if s, ok := v.AsText(); ok && v.Type() == driver.TypeString {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// matchesAll applies every condition to a row.
func matchesAll(cols []column, row driver.Row, conds []driver.Condition) (bool, error) {
	for _, cond := range conds {
		i := indexOf(cols, cond.Field)
		if i < 0 {
			return false, fmt.Errorf("unknown field %q", cond.Field)
		}
		ok, err := matches(row[i], cond)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func indexOf(cols []column, name string) int {
	for i, c := range cols {
		if c.name == name {
			return i
		}
	}
	return -1
}
