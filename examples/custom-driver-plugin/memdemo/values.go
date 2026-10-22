package memdemo

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// number returns an Int, Float or Decimal value as a float64.
func number(v driver.Value) (float64, bool) {
	if f, ok := v.AsFloat(); ok {
		return f, true
	}
	if v.Type() == driver.TypeDecimal {
		s, _ := v.AsText()
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

// compare orders two non-NULL values of comparable types: numbers with
// numbers, times with times, booleans with booleans (false first) and
// text with text. ok is false for anything else, NULL included.
func compare(a, b driver.Value) (int, bool) {
	if x, ok := a.AsInt(); ok {
		if y, ok := b.AsInt(); ok {
			return cmp.Compare(x, y), true
		}
	}
	if x, ok := number(a); ok {
		if y, ok := number(b); ok {
			return cmp.Compare(x, y), true
		}
		return 0, false
	}
	if x, ok := a.AsTime(); ok {
		if y, ok := b.AsTime(); ok {
			return x.Compare(y), true
		}
		return 0, false
	}
	if x, ok := a.AsBool(); ok {
		if y, ok := b.AsBool(); ok {
			return cmp.Compare(boolInt(x), boolInt(y)), true
		}
		return 0, false
	}
	if a.Type() == driver.TypeString && b.Type() == driver.TypeString {
		x, _ := a.AsText()
		y, _ := b.AsText()
		return strings.Compare(x, y), true
	}
	return 0, false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// equal reports whether two values compare equal. NULL equals nothing, as
// in SQL. Values compare cannot order (JSON, say) are equal when their
// type and text are.
func equal(a, b driver.Value) bool {
	if a.IsNull() || b.IsNull() {
		return false
	}
	if c, ok := compare(a, b); ok {
		return c == 0
	}
	return a.Type() == b.Type() && a.String() == b.String()
}

// text is a value's text for contains and prefix: the string itself, or
// the display form of anything else (an id 12 is "12").
func text(v driver.Value) string {
	if s, ok := v.AsText(); ok {
		return s
	}
	return v.String()
}

// matches reports whether a cell satisfies a filter condition.
func matches(cell driver.Value, c driver.Condition) (bool, error) {
	switch c.Op {
	case driver.OpIsNull:
		return cell.IsNull(), nil
	case driver.OpNotNull:
		return !cell.IsNull(), nil
	}
	if cell.IsNull() || c.Value.IsNull() {
		return false, nil
	}
	switch c.Op {
	case driver.OpEq:
		return equal(cell, c.Value), nil
	case driver.OpNe:
		return !equal(cell, c.Value), nil
	case driver.OpContains:
		return strings.Contains(text(cell), text(c.Value)), nil
	case driver.OpPrefix:
		return strings.HasPrefix(text(cell), text(c.Value)), nil
	case driver.OpIn:
		list, ok := c.Value.AsList()
		if !ok {
			return false, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("%s in: want a list", c.Field))
		}
		return slices.ContainsFunc(list, func(v driver.Value) bool { return equal(cell, v) }), nil
	case driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		n, ok := compare(cell, c.Value)
		if !ok {
			return false, driver.Wrap(driver.ErrSyntax, Name, "browse",
				fmt.Errorf("%s %s: cannot compare %s with %s", c.Field, c.Op, cell.Type(), c.Value.Type()))
		}
		switch c.Op {
		case driver.OpLt:
			return n < 0, nil
		case driver.OpLe:
			return n <= 0, nil
		case driver.OpGt:
			return n > 0, nil
		default:
			return n >= 0, nil
		}
	}
	return false, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("filter operator %q", c.Op))
}
