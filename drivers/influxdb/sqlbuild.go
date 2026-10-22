package influxdb

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Statement building for Browse and Range. Identifiers come from Describe
// and are double-quoted (a '"' in a name is doubled); every filter value is
// a query parameter ($1, $2…), except times, which are rendered as
// to_timestamp_nanos(<integer>) so they compare as timestamps.

const (
	defaultPage = 100
	maxPage     = 10_000
)

// table is a measurement as Describe reports it.
type table struct {
	ref    driver.CollectionRef
	fields []driver.Field
}

// tags lists the tag columns in table order.
func (t *table) tags() []string {
	var out []string
	for _, f := range t.fields {
		if f.Comment == "tag" {
			out = append(out, f.Name)
		}
	}
	return out
}

// resolve finds the column named name: exactly, else case-insensitively
// when that is unambiguous. Anything else is ErrNotFound.
func (t *table) resolve(op, name string) (driver.Field, error) {
	var folded []driver.Field
	for _, f := range t.fields {
		if f.Name == name {
			return f, nil
		}
		if strings.EqualFold(f.Name, name) {
			folded = append(folded, f)
		}
	}
	if len(folded) == 1 {
		return folded[0], nil
	}
	why := "no column %q in %s"
	if len(folded) > 1 {
		why = "column %q is ambiguous in %s"
	}
	return driver.Field{}, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf(why, name, t.ref.Name))
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// timeLiteral renders t as a nanosecond timestamp. Outside the years
// 1678-2262 nanoseconds since the epoch overflow int64, so such a time is
// ErrSyntax rather than a wrong instant.
func timeLiteral(op string, t time.Time) (string, error) {
	ns := t.UnixNano()
	if !time.Unix(0, ns).Equal(t) {
		return "", driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("time %s is outside the range InfluxDB can store (1678-2262)", t.UTC().Format(time.RFC3339)))
	}
	return "to_timestamp_nanos(" + strconv.FormatInt(ns, 10) + ")", nil
}

// statement is a built query with its parameters.
type statement struct {
	sql    string
	params map[string]any
	limit  int // Browse: page size
	offset int // Browse: rows skipped
}

// builder accumulates WHERE terms and parameters.
type builder struct {
	op     string
	params map[string]any
	where  []string
}

func (b *builder) param(v any) string {
	if b.params == nil {
		b.params = map[string]any{}
	}
	key := strconv.Itoa(len(b.params) + 1)
	b.params[key] = v
	return "$" + key
}

// value renders v as a parameter or, for a time, a literal.
func (b *builder) value(v driver.Value) (string, error) {
	if t, ok := v.AsTime(); ok {
		return timeLiteral(b.op, t)
	}
	p, err := paramValue(b.op, v)
	if err != nil {
		return "", err
	}
	return b.param(p), nil
}

var compareOps = map[driver.Op]string{
	driver.OpEq: "=", driver.OpNe: "<>", driver.OpLt: "<", driver.OpLe: "<=", driver.OpGt: ">", driver.OpGe: ">=",
}

// filter adds f's conditions and its raw fragment to the WHERE terms.
func (b *builder) filter(t *table, f driver.Filter) error {
	for _, c := range f.Conditions {
		field, err := t.resolve(b.op, c.Field)
		if err != nil {
			return err
		}
		col := quoteIdent(field.Name)
		term, err := b.condition(col, c)
		if err != nil {
			return err
		}
		b.where = append(b.where, term)
	}
	if f.Raw != "" {
		if err := checkRawFilter(f.Raw); err != nil {
			return err
		}
		// The newline ends the fragment before the closing parenthesis.
		b.where = append(b.where, "("+f.Raw+"\n)")
	}
	return nil
}

func (b *builder) condition(col string, c driver.Condition) (string, error) {
	bad := func(kind error, format string, args ...any) (string, error) {
		return "", driver.Wrap(kind, Name, b.op, fmt.Errorf(format, args...))
	}
	switch c.Op {
	case driver.OpIsNull:
		return col + " IS NULL", nil
	case driver.OpNotNull:
		return col + " IS NOT NULL", nil
	case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		if c.Value.IsNull() {
			switch c.Op {
			case driver.OpEq:
				return col + " IS NULL", nil
			case driver.OpNe:
				return col + " IS NOT NULL", nil
			}
			return bad(driver.ErrSyntax, "operator %s cannot compare with null", c.Op)
		}
		v, err := b.value(c.Value)
		if err != nil {
			return "", err
		}
		return col + " " + compareOps[c.Op] + " " + v, nil
	case driver.OpContains, driver.OpPrefix:
		text, ok := c.Value.AsText()
		if !ok || c.Value.Type() != driver.TypeString {
			text = c.Value.String()
		}
		p := b.param(text)
		if c.Op == driver.OpContains {
			return "strpos(CAST(" + col + " AS VARCHAR), " + p + ") > 0", nil
		}
		return "starts_with(CAST(" + col + " AS VARCHAR), " + p + ")", nil
	case driver.OpIn:
		list, ok := c.Value.AsList()
		if !ok {
			return bad(driver.ErrSyntax, "operator in needs a list value")
		}
		if len(list) == 0 {
			return "FALSE", nil
		}
		vals := make([]string, len(list))
		for i, v := range list {
			s, err := b.value(v)
			if err != nil {
				return "", err
			}
			vals[i] = s
		}
		return col + " IN (" + strings.Join(vals, ", ") + ")", nil
	}
	return bad(driver.ErrUnsupported, "operator %q is not supported", c.Op)
}

func (b *builder) whereClause() string {
	if len(b.where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(b.where, " AND ")
}

// buildBrowse builds one Browse page: newest first, then the tags, which
// with the time make the series key and so a stable order for OFFSET.
func buildBrowse(t *table, opts driver.BrowseOptions) (statement, error) {
	b := &builder{op: "browse"}
	offset, err := parseCursor(opts.Cursor)
	if err != nil {
		return statement{}, err
	}
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = defaultPage
	case limit > maxPage:
		limit = maxPage
	}
	if err := b.filter(t, opts.Filter); err != nil {
		return statement{}, err
	}
	var order []string
	seen := map[string]bool{}
	for _, k := range opts.Sort {
		f, err := t.resolve(b.op, k.Field)
		if err != nil {
			return statement{}, err
		}
		if seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		dir := ""
		if k.Desc {
			dir = " DESC"
		}
		order = append(order, quoteIdent(f.Name)+dir)
	}
	if !seen["time"] {
		order = append(order, `"time" DESC`)
		seen["time"] = true
	}
	for _, tag := range t.tags() {
		if !seen[tag] {
			order = append(order, quoteIdent(tag))
		}
	}
	sql := "SELECT * FROM " + quoteIdent(t.ref.Name) + b.whereClause() +
		" ORDER BY " + strings.Join(order, ", ") +
		" LIMIT " + strconv.Itoa(limit+1) + " OFFSET " + strconv.Itoa(offset)
	return statement{sql: sql, params: b.params, limit: limit, offset: offset}, nil
}

// parseCursor reads an "o:<offset>" cursor; "" is the first page.
func parseCursor(c string) (int, error) {
	if c == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(c, "o:"))
	if !strings.HasPrefix(c, "o:") || err != nil || n < 0 {
		return 0, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("invalid cursor %q", c))
	}
	return n, nil
}

func cursorAt(offset int) string { return "o:" + strconv.Itoa(offset) }

// aggregates maps a RangeQuery.Aggregate to its SQL; %s is the field.
var aggregates = map[string]string{
	"mean":  "avg(%s)",
	"sum":   "sum(%s)",
	"min":   "min(%s)",
	"max":   "max(%s)",
	"count": "count(%s)",
	"last":  `last_value(%s ORDER BY "time")`,
}

// buildRange builds a Range statement: raw points of one field when Window
// is 0, else one row per date_bin window (and group), aggregated. Windows
// are aligned to the Unix epoch in UTC, _time is a window's start, and
// empty windows are absent (GROUP BY yields no row for them). End is
// exclusive.
func buildRange(t *table, q driver.RangeQuery) (statement, error) {
	b := &builder{op: "range"}
	bad := func(kind error, format string, args ...any) (statement, error) {
		return statement{}, driver.Wrap(kind, Name, b.op, fmt.Errorf(format, args...))
	}
	if q.Field == "" {
		return bad(driver.ErrSyntax, "Range needs a field")
	}
	field, err := t.resolve(b.op, q.Field)
	if err != nil {
		return statement{}, err
	}
	if field.Name == "time" {
		return bad(driver.ErrSyntax, "the time column is not a value field")
	}
	if q.Window < 0 {
		return bad(driver.ErrSyntax, "negative window %s", q.Window)
	}
	if !q.Start.IsZero() && !q.End.IsZero() && !q.End.After(q.Start) {
		return bad(driver.ErrSyntax, "range end %s is not after its start %s", q.End, q.Start)
	}
	var groups []string
	for _, g := range q.GroupBy {
		f, err := t.resolve(b.op, g)
		if err != nil {
			return statement{}, err
		}
		groups = append(groups, quoteIdent(f.Name))
	}
	for _, bound := range []struct {
		t  time.Time
		op string
	}{{q.Start, ">="}, {q.End, "<"}} {
		if bound.t.IsZero() {
			continue
		}
		lit, err := timeLiteral(b.op, bound.t)
		if err != nil {
			return statement{}, err
		}
		b.where = append(b.where, `"time" `+bound.op+" "+lit)
	}
	if err := b.filter(t, q.Filter); err != nil {
		return statement{}, err
	}
	col := quoteIdent(field.Name)
	from := " FROM " + quoteIdent(t.ref.Name)
	keys := append([]string{`"_time"`}, groups...)
	// Rows without this field are dropped before windowing: InfluxDB
	// measurements are sparse, and a window holding only other fields must
	// be absent, not NULL or 0; last_value must also skip them.
	b.where = append(b.where, col+" IS NOT NULL")
	if q.Window == 0 {
		sel := append(append([]string{`"time" AS "_time"`}, groups...), col+` AS "value"`)
		sql := "SELECT " + strings.Join(sel, ", ") + from + b.whereClause() + " ORDER BY " + strings.Join(keys, ", ")
		return statement{sql: sql, params: b.params}, nil
	}
	agg := strings.ToLower(q.Aggregate)
	if agg == "" {
		agg = "mean"
	}
	tmpl, ok := aggregates[agg]
	if !ok {
		return bad(driver.ErrUnsupported, "aggregate %q is not supported (mean, sum, min, max, count, last)", q.Aggregate)
	}
	// date_bin's default origin is the Unix epoch (UTC), as RangeQuery asks.
	bin := "date_bin(INTERVAL '" + strconv.FormatInt(q.Window.Nanoseconds(), 10) + ` nanoseconds', "time") AS "_time"`
	sel := append(append([]string{bin}, groups...), fmt.Sprintf(tmpl, col)+` AS "value"`)
	sql := "SELECT " + strings.Join(sel, ", ") + from + b.whereClause() +
		" GROUP BY " + strings.Join(keys, ", ") + " ORDER BY " + strings.Join(keys, ", ")
	return statement{sql: sql, params: b.params}, nil
}

// paramValue converts a Value to a query parameter, which travels as JSON.
func paramValue(op string, v driver.Value) (any, error) {
	unsupported := func(why string) (any, error) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(why))
	}
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
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return unsupported("NaN and infinite parameters are not supported")
		}
		return f, nil
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	return unsupported(v.Type().String() + " parameters are not supported")
}

// queryParams turns positional Query.Params into InfluxDB's named
// parameters "1", "2"…, which the text references as $1, $2….
func queryParams(vals []driver.Value) (map[string]any, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(vals))
	for i, v := range vals {
		p, err := paramValue("query", v)
		if err != nil {
			return nil, err
		}
		out[strconv.Itoa(i+1)] = p
	}
	return out, nil
}
