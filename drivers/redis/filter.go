package redis

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// browseCursor is the decoded BrowseOptions.Cursor.
//
//   - "k:<base64url key>": unsorted paging, keyset by key name. The next
//     page holds the keys strictly greater (in byte order) than the last
//     key returned, so a SCAN that repeats or reorders keys between pages
//     (a rehash) cannot repeat or skip one. The key is at most maxCursorKey
//     bytes.
//   - "o:<offset>": sorted paging over the bounded full scan.
type browseCursor struct {
	sorted   bool
	offset   int
	after    string // the last key returned, when hasAfter
	hasAfter bool
}

// maxCursorKey bounds the key a cursor carries, so a token stays small.
const maxCursorKey = 8192

// encode returns the cursor's token. A key longer than maxCursorKey cannot
// be carried: ErrUnsupported.
func (c browseCursor) encode() (string, error) {
	if c.sorted {
		return "o:" + strconv.Itoa(c.offset), nil
	}
	if !c.hasAfter {
		return "", nil
	}
	if len(c.after) > maxCursorKey {
		return "", driver.Wrap(driver.ErrUnsupported, Name, "browse",
			fmt.Errorf("the page ends at a key longer than %d bytes, which a cursor cannot carry; narrow the browse with a filter pattern", maxCursorKey))
	}
	return "k:" + base64.RawURLEncoding.EncodeToString([]byte(c.after)), nil
}

func parseCursor(s string) (browseCursor, error) {
	if s == "" {
		return browseCursor{}, nil
	}
	bad := func() (browseCursor, error) {
		shown := s
		if len(shown) > 40 {
			shown = shown[:40] + "…"
		}
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("invalid cursor %q", shown))
	}
	kind, rest, ok := strings.Cut(s, ":")
	switch {
	case !ok:
		return bad()
	case kind == "o":
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 {
			return bad()
		}
		return browseCursor{sorted: true, offset: n}, nil
	case kind == "k":
		if len(rest) > base64.RawURLEncoding.EncodedLen(maxCursorKey) {
			return bad()
		}
		k, err := base64.RawURLEncoding.DecodeString(rest)
		if err != nil {
			return bad()
		}
		return browseCursor{after: string(k), hasAfter: true}, nil
	}
	return bad()
}

// matchConditions reports whether row satisfies every condition. A field
// the row does not have is NULL.
func matchConditions(cols []driver.Column, row driver.Row, conds []driver.Condition) (bool, error) {
	for _, c := range conds {
		v := driver.Null()
		for i, col := range cols {
			if col.Name == c.Field && i < len(row) {
				v = row[i]
				break
			}
		}
		ok, err := matchCondition(v, c)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func matchCondition(v driver.Value, c driver.Condition) (bool, error) {
	switch c.Op {
	case driver.OpIsNull:
		return v.IsNull(), nil
	case driver.OpNotNull:
		return !v.IsNull(), nil
	case driver.OpIn:
		list, ok := c.Value.AsList()
		if !ok {
			return false, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("the in operator takes a list"))
		}
		for _, w := range list {
			if !v.IsNull() && compareValues(v, w) == 0 {
				return true, nil
			}
		}
		return false, nil
	case driver.OpContains, driver.OpPrefix:
		if v.IsNull() {
			return false, nil
		}
		text, needle := displayText(v), displayText(c.Value)
		if c.Op == driver.OpContains {
			return strings.Contains(text, needle), nil
		}
		return strings.HasPrefix(text, needle), nil
	case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		if v.IsNull() || c.Value.IsNull() {
			return false, nil // SQL semantics: a comparison with NULL is never true
		}
		n := compareValues(v, c.Value)
		switch c.Op {
		case driver.OpEq:
			return n == 0, nil
		case driver.OpNe:
			return n != 0, nil
		case driver.OpLt:
			return n < 0, nil
		case driver.OpLe:
			return n <= 0, nil
		case driver.OpGt:
			return n > 0, nil
		}
		return n >= 0, nil
	}
	return false, driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("filter operator %q is not supported", c.Op))
}

// displayText is the text a value is matched on by contains and prefix.
func displayText(v driver.Value) string {
	if s, ok := v.AsText(); ok {
		return s
	}
	if b, ok := v.AsBytes(); ok {
		return string(b)
	}
	if t, ok := v.AsTime(); ok {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return v.String()
}

// compareValues orders two non-NULL values: numerically when both are
// numbers (or numeric text), as instants when one is a time, as booleans,
// and otherwise by their text.
func compareValues(a, b driver.Value) int {
	if ra, ok := ratOf(a); ok {
		if rb, ok := ratOf(b); ok {
			return ra.Cmp(rb)
		}
	}
	if ta, ok := timeOf(a); ok {
		if tb, ok := timeOf(b); ok {
			return ta.Compare(tb)
		}
	}
	if ba, ok := a.AsBool(); ok {
		if bb, ok := boolOf(b); ok {
			return boolCmp(ba, bb)
		}
	}
	return strings.Compare(displayText(a), displayText(b))
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

func boolOf(v driver.Value) (bool, bool) {
	if b, ok := v.AsBool(); ok {
		return b, true
	}
	if s, ok := v.AsText(); ok && (s == "true" || s == "false") {
		return s == "true", true
	}
	return false, false
}

func ratOf(v driver.Value) (*big.Rat, bool) {
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
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		if v.Type() == driver.TypeString && !isCanonicalInt(s) && !isDecimal(s) {
			return nil, false
		}
		return new(big.Rat).SetString(s)
	}
	return nil, false
}

func timeOf(v driver.Value) (time.Time, bool) {
	if t, ok := v.AsTime(); ok {
		return t, true
	}
	if s, ok := v.AsText(); ok && isTime(s) {
		t, err := time.Parse(time.RFC3339Nano, s)
		return t, err == nil
	}
	return time.Time{}, false
}

// compareForSort orders values for Browse sorting, with NULLs last.
func compareForSort(a, b driver.Value) int {
	switch {
	case a.IsNull() && b.IsNull():
		return 0
	case a.IsNull():
		return 1
	case b.IsNull():
		return -1
	}
	return compareValues(a, b)
}

// detectIDField returns the hash field that mirrors the key: the field
// whose value equals the key's suffix after "<coll>:" in every sampled
// record. "id" wins when several do; "" when none does.
func detectIDField(coll string, names, keys []string, recs []map[string]string) string {
	if len(keys) == 0 {
		return ""
	}
	var found []string
	for _, name := range names {
		ok := true
		for i, k := range keys {
			suffix, has := strings.CutPrefix(k, coll+":")
			if !has || recs[i][name] != suffix {
				ok = false
				break
			}
		}
		if ok {
			found = append(found, name)
		}
	}
	for _, f := range found {
		if f == "id" {
			return f
		}
	}
	if len(found) > 0 {
		return found[0]
	}
	return ""
}
