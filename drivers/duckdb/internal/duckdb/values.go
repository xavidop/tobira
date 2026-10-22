package duckdb

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/xavidop/tobira/driver"
)

// Caps on one value, below the plugin protocol's 1 MiB per value (nested
// members included): a longer string or blob is cut, a longer list or map
// keeps its first items, and nesting past maxDepth is shown as text. On top
// of these, the whole value (every list item, map entry and struct member,
// at any depth) shares valueBudget bytes of estimated encoding: once spent,
// strings are cut shorter and lists, maps and structs keep only the items
// that fit. A cut adds a warning to the result's Stats.
const (
	maxValueBytes = 512 << 10
	maxListItems  = 10000
	maxDepth      = 32
	valueBudget   = 960 << 10 // 1 MiB less headroom for the estimate
	nodeCost      = 16        // an upper bound on one encoded node's framing
)

// converter turns the Go values duckdb-go scans into driver values, with
// the caps above. cut records that something was cut; left is what remains
// of the current value's budget.
type converter struct {
	cut  bool
	left int
}

// value converts v, scanned from a column of type n.
func (c *converter) value(n *typeNode, v any) driver.Value {
	c.left = valueBudget
	return c.convert(n, v, 0)
}

// spend charges n bytes against the value's budget.
func (c *converter) spend(n int) { c.left -= n + nodeCost }

// full reports whether no further item fits; it marks the value cut.
func (c *converter) full() bool {
	if c.left < 4*nodeCost {
		c.cut = true
		return true
	}
	return false
}

func (c *converter) convert(n *typeNode, v any, depth int) driver.Value {
	out := c.convertValue(n, v, depth)
	switch out.Type() {
	case driver.TypeList, driver.TypeObject: // charged item by item
		c.spend(0)
	case driver.TypeString, driver.TypeDecimal:
		s, _ := out.AsText()
		c.spend(len(s))
	case driver.TypeBytes:
		b, _ := out.AsBytes()
		c.spend(len(b))
	case driver.TypeJSON:
		b, _ := out.AsJSON()
		c.spend(len(b))
	case driver.TypeTime:
		c.spend(2 * nodeCost)
	default:
		c.spend(0)
	}
	return out
}

func (c *converter) convertValue(n *typeNode, v any, depth int) driver.Value {
	if v == nil {
		return driver.Null()
	}
	if depth > maxDepth {
		c.cut = true
		return driver.String(c.text(fmt.Sprint(v)))
	}
	if n == nil {
		n = &typeNode{kind: "scalar"}
	}
	switch x := v.(type) {
	case bool:
		return driver.Bool(x)
	case int8:
		return driver.Int(int64(x))
	case int16:
		return driver.Int(int64(x))
	case int32:
		return driver.Int(int64(x))
	case int64:
		return driver.Int(x)
	case uint8:
		return driver.Int(int64(x))
	case uint16:
		return driver.Int(int64(x))
	case uint32:
		return driver.Int(int64(x))
	case uint64:
		if nodeType(n) == driver.TypeInt && x <= 1<<63-1 {
			return driver.Int(int64(x))
		}
		return driver.Decimal(strconv.FormatUint(x, 10))
	case float32:
		return driver.Float(float64(x))
	case float64:
		return driver.Float(x)
	case *big.Int:
		return driver.Decimal(x.String())
	case duckdb.Decimal:
		return driver.Decimal(decimalText(x))
	case duckdb.Interval:
		return driver.String(intervalText(x))
	case duckdb.UUID:
		return driver.String(uuidText(x[:]))
	case string:
		if n.kind == "scalar" && n.name == "JSON" {
			return c.json(x)
		}
		return driver.String(c.text(x))
	case []byte:
		if n.kind == "scalar" && n.name == "UUID" && len(x) == 16 {
			return driver.String(uuidText(x))
		}
		if limit := c.limit(); len(x) > limit {
			c.cut = true
			x = x[:limit]
		}
		return driver.Bytes(x)
	case time.Time:
		return driver.Time(x)
	case []any:
		if n.kind == "scalar" && n.name == "JSON" {
			return c.jsonValue(x)
		}
		return c.list(n, x, depth)
	case map[string]any:
		if n.kind == "scalar" && n.name == "JSON" {
			return c.jsonValue(x)
		}
		return c.structValue(n, x, depth)
	case duckdb.OrderedMap:
		return c.mapValue(n, x.Keys(), x.Values(), depth)
	case duckdb.Map:
		keys := make([]any, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		vals := make([]any, len(keys))
		for i, k := range keys {
			vals[i] = x[k]
		}
		return c.mapValue(n, keys, vals, depth)
	case duckdb.Union:
		var member *typeNode
		for _, f := range n.fields {
			if f.name == x.Tag {
				member = f.typ
			}
		}
		return driver.ObjectValue(driver.Member{Key: x.Tag, Value: c.convert(member, x.Value, depth+1)})
	case duckdb.Bit:
		return driver.String(string(x.Data))
	}
	if n.kind == "scalar" && n.name == "JSON" { // a JSON scalar: number, bool
		return c.jsonValue(v)
	}
	return driver.String(c.text(fmt.Sprint(v)))
}

// limit is the most bytes one string or blob may keep: maxValueBytes, or
// what is left of the value's budget.
func (c *converter) limit() int {
	return max(0, min(maxValueBytes, c.left-2*nodeCost))
}

// text cuts s to limit() on a rune boundary.
func (c *converter) text(s string) string {
	limit := c.limit()
	if len(s) <= limit {
		return s
	}
	c.cut = true
	end := limit
	for end > 0 && s[end]&0xC0 == 0x80 {
		end--
	}
	return s[:end] + "…"
}

func (c *converter) json(s string) driver.Value {
	if len(s) > c.limit() {
		c.cut = true
		return driver.String(c.text(s))
	}
	return driver.JSON([]byte(s))
}

// jsonValue re-encodes a JSON value duckdb-go has already decoded. Object
// keys come back sorted (encoding/json), and numbers past 2^53 lose
// precision: duckdb-go decodes them as float64.
func (c *converter) jsonValue(v any) driver.Value {
	b, err := json.Marshal(v)
	if err != nil {
		return driver.String(c.text(fmt.Sprint(v)))
	}
	return c.json(string(b))
}

func (c *converter) list(n *typeNode, items []any, depth int) driver.Value {
	elem := n.elem
	if len(items) > maxListItems {
		c.cut = true
		items = items[:maxListItems]
	}
	out := make([]driver.Value, 0, len(items))
	for _, it := range items {
		if c.full() {
			break
		}
		out = append(out, c.convert(elem, it, depth+1))
	}
	return driver.List(out...)
}

// structValue orders a STRUCT's members as the type declares them; keys
// the type does not name (or a type that did not parse) follow sorted.
func (c *converter) structValue(n *typeNode, m map[string]any, depth int) driver.Value {
	members := make([]driver.Member, 0, len(m))
	seen := make(map[string]bool, len(m))
	for _, f := range n.fields {
		if v, ok := m[f.name]; ok {
			seen[f.name] = true
			if c.full() {
				continue
			}
			c.spend(len(f.name))
			members = append(members, driver.Member{Key: f.name, Value: c.convert(f.typ, v, depth+1)})
		}
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		if c.full() {
			break
		}
		c.spend(len(k))
		members = append(members, driver.Member{Key: k, Value: c.convert(nil, m[k], depth+1)})
	}
	return driver.ObjectValue(members...)
}

// mapValue renders a MAP as an object in map order, each key as its text.
func (c *converter) mapValue(n *typeNode, keys, vals []any, depth int) driver.Value {
	if len(keys) > maxListItems {
		c.cut = true
		keys, vals = keys[:maxListItems], vals[:maxListItems]
	}
	members := make([]driver.Member, 0, len(keys))
	for i, k := range keys {
		if c.full() {
			break
		}
		kv := c.convert(n.key, k, depth+1)
		members = append(members, driver.Member{Key: kv.String(), Value: c.convert(n.value, vals[i], depth+1)})
	}
	return driver.ObjectValue(members...)
}

// decimalText renders a DECIMAL exactly, with its scale: 12.50, -0.05.
func decimalText(d duckdb.Decimal) string {
	if d.Value == nil {
		return "0"
	}
	s := new(big.Int).Abs(d.Value).String()
	neg := d.Value.Sign() < 0
	if scale := int(d.Scale); scale > 0 {
		if len(s) <= scale {
			s = strings.Repeat("0", scale-len(s)+1) + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// intervalText renders an INTERVAL the way DuckDB prints it:
// "1 year 2 months 3 days 00:00:05.25".
func intervalText(iv duckdb.Interval) string {
	var parts []string
	unit := func(n int64, one, many string) {
		if n == 0 {
			return
		}
		if n == 1 || n == -1 {
			parts = append(parts, strconv.FormatInt(n, 10)+" "+one)
			return
		}
		parts = append(parts, strconv.FormatInt(n, 10)+" "+many)
	}
	unit(int64(iv.Months)/12, "year", "years")
	unit(int64(iv.Months)%12, "month", "months")
	unit(int64(iv.Days), "day", "days")
	if iv.Micros != 0 || len(parts) == 0 {
		us := iv.Micros
		sign := ""
		if us < 0 {
			sign, us = "-", -us
		}
		h, m := us/3_600_000_000, us/60_000_000%60
		sec, frac := us/1_000_000%60, us%1_000_000
		clock := fmt.Sprintf("%s%02d:%02d:%02d", sign, h, m, sec)
		if frac != 0 {
			clock += strings.TrimRight(fmt.Sprintf(".%06d", frac), "0")
		}
		parts = append(parts, clock)
	}
	return strings.Join(parts, " ")
}

// uuidText formats 16 bytes as 8-4-4-4-12 hex.
func uuidText(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// toArg converts a driver value to a query parameter duckdb-go binds.
func toArg(v driver.Value) any {
	switch v.Type() {
	case driver.TypeNull:
		return nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b
	case driver.TypeInt:
		i, _ := v.AsInt()
		return i
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t
	case driver.TypeJSON:
		b, _ := v.AsJSON()
		return string(b)
	}
	return v.String()
}

func toArgs(params []driver.Value) []any {
	args := make([]any, len(params))
	for i, p := range params {
		args[i] = toArg(p)
	}
	return args
}
