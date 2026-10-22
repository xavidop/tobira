package d1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

var errNoTransactions = errors.New("D1's REST API has no transactions: each statement commits on its own")

// maxSafeInt is the largest integer a JavaScript number holds exactly.
const maxSafeInt = 1<<53 - 1

// toParam converts a parameter to its JSON form for D1. Times are UTC text
// and decimals, JSON and strings are text, as in the sqlite driver; a bool
// is 0 or 1. An integer beyond ±2^53 goes as decimal text, since D1's
// JavaScript would round a number (SQLite's affinity turns the text back
// into an integer for an INTEGER column). Blobs have no documented JSON
// form, so they are refused.
func toParam(op string, i int, v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		if b {
			return 1, nil
		}
		return 0, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		if n > maxSafeInt || n < -maxSafeInt {
			return v.String(), nil
		}
		return n, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("parameter %d: NaN and infinities have no JSON form", i+1))
		}
		return f, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC().Format("2006-01-02 15:04:05.999999999"), nil
	case driver.TypeBytes, driver.TypeVector, driver.TypeList, driver.TypeObject:
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("parameter %d: %s values cannot be bound over the D1 REST API", i+1, v.Type()))
	}
	return v.String(), nil
}

func params(op string, vs []driver.Value) ([]any, error) {
	if len(vs) == 0 {
		return nil, nil
	}
	out := make([]any, len(vs))
	for i, v := range vs {
		p, err := toParam(op, i, v)
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

// Query runs one statement as one request. On a read-only connection, or
// with Query.ReadOnly, the statement must pass sqlitecore.ReadOnlyGuard:
// D1 has no read-only session or transaction, so that lexical gate is the
// only layer. Transaction control is refused: D1's REST API has none.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.query(ctx, q, nil)
}

// query runs q; schema, when known (Browse), types its columns without a
// lookup.
func (c *conn) query(ctx context.Context, q driver.Query, schema *driver.CollectionSchema) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	if c.readOnly || q.ReadOnly {
		if err := sqlitecore.ReadOnlyGuard(Name, q.Text); err != nil {
			return nil, err
		}
	}
	if err := sqlitecore.CheckSingle(Name, q.Text); err != nil {
		return nil, err
	}
	if sqlitecore.IsTxControl(q.Text) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errNoTransactions)
	}
	args, err := params(op, q.Params)
	if err != nil {
		return nil, err
	}
	var declared func(n int) []string
	if schema != nil {
		declared = func(n int) []string { return starTypes(schema, n) }
	} else if declared, err = c.sourceTypes(ctx, q.Text); err != nil {
		return nil, err
	}
	r, err := c.client.exec(ctx, q.Text, args)
	if err != nil {
		return nil, mapError(op, err)
	}
	var types []string
	if declared != nil {
		types = declared(len(r.Columns))
	}
	return resultSet(r, types, q.Limit, start), nil
}

// sourceTypes returns, for a plain SELECT of columns (or *) from one table
// of main (see simpleSource), a function giving the declared type of each
// result column. D1 reports no column types, so this is the only way a
// query's BOOLEAN, DATETIME, DECIMAL or JSON columns come back typed;
// anything else is typed from its values. A failed lookup is ignored (the
// statement then reports its own error), except a cancelled ctx.
func (c *conn) sourceTypes(ctx context.Context, text string) (func(int) []string, error) {
	table, cols, ok := simpleSource(text)
	if !ok {
		return nil, nil
	}
	r, err := c.client.exec(ctx, "PRAGMA table_xinfo("+sqlitecore.QuoteIdent(table)+")", nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, mapError("query", ctx.Err())
		}
		return nil, nil
	}
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Namespace: "main", Name: table}, Type: driver.CollectionTable}
	sqlitecore.ApplyTableXInfo(s, r.Rows)
	if cols == nil {
		return func(n int) []string { return starTypes(s, n) }, nil
	}
	return func(n int) []string {
		if n != len(cols) {
			return nil
		}
		out := make([]string, n)
		for i, name := range cols {
			for _, f := range s.Fields {
				if strings.EqualFold(f.Name, name) {
					out[i] = f.NativeType
					break
				}
			}
		}
		return out
	}, nil
}

// starTypes is the declared types of "SELECT *" over schema's table, when
// the result has one column per field.
func starTypes(schema *driver.CollectionSchema, n int) []string {
	if n != len(schema.Fields) {
		return nil
	}
	out := make([]string, n)
	for i, f := range schema.Fields {
		out[i] = f.NativeType
	}
	return out
}

// resultSet converts a statement result. One without columns reports
// meta.changes as RowsAffected; one with columns is limited to limit rows
// (Truncated when more came back). A column with a declared type is typed
// by it; any other takes the first row's runtime type, and its values are
// converted per row.
func resultSet(r *stmtResult, declared []string, limit int, start time.Time) driver.ResultSet {
	if len(r.Columns) == 0 {
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: max(r.Meta.Changes, 0), Duration: time.Since(start)})
	}
	raw := r.Rows
	truncated := false
	if limit > 0 && len(raw) > limit {
		raw, truncated = raw[:limit], true
	}
	if len(declared) != len(r.Columns) {
		declared = make([]string, len(r.Columns))
	}
	cols := make([]driver.Column, len(r.Columns))
	types := make([]driver.Type, len(r.Columns))
	for i, name := range r.Columns {
		declared[i] = strings.TrimSpace(declared[i])
		types[i] = sqlitecore.MapType(declared[i])
		if declared[i] == "" && len(raw) > 0 {
			types[i] = sqlitecore.ColumnType("", raw[0][i])
		}
		cols[i] = driver.Column{Name: name, NativeType: declared[i], Type: types[i]}
	}
	rows := make([]driver.Row, len(raw))
	for j, vals := range raw {
		row := make(driver.Row, len(vals))
		for i, v := range vals {
			row[i] = convert(declared[i], types[i], v)
		}
		rows[j] = row
	}
	return driver.NewSliceResultSet(cols, rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start), Truncated: truncated})
}

// convert is sqlitecore's conversion, plus one D1 quirk: JavaScript writes a
// REAL that is whole (3.0) as an integer (3), so an integer in a REAL column
// is a float again.
func convert(declared string, typ driver.Type, v any) driver.Value {
	if declared == "" {
		return sqlitecore.ConvertValue("", v)
	}
	if n, ok := v.(int64); ok && typ == driver.TypeFloat {
		return driver.Float(float64(n))
	}
	return sqlitecore.ConvertTyped(declared, typ, v)
}

// simpleSource recognises "SELECT [DISTINCT|ALL] <* | col, …> FROM
// [main.]<table> [[AS] alias] [WHERE|GROUP|ORDER|LIMIT …]" with no compound
// operator (UNION, INTERSECT, EXCEPT) outside parentheses, where each col
// is a bare or quoted name, optionally qualified by the table or alias. It
// returns the table and the column names (nil for *). Any other shape
// (an alias on a column, an expression, a join, a subquery in FROM, a CTE)
// is not recognised, and the result is typed from its values.
func simpleSource(text string) (table string, cols []string, ok bool) {
	toks := tokens(text)
	i := 0
	peekWord := func() string {
		if i < len(toks) && toks[i].Kind == sqlscan.Word {
			return strings.ToUpper(text[toks[i].Start:toks[i].End])
		}
		return ""
	}
	if w := peekWord(); w != "SELECT" {
		return "", nil, false
	}
	i++
	if w := peekWord(); w == "DISTINCT" || w == "ALL" {
		i++
	}
	type colRef struct{ qual, name string }
	var refs []colRef
	star := i < len(toks) && toks[i].Kind == sqlscan.Punct && text[toks[i].Start:toks[i].End] == "*"
	if star {
		i++
	} else {
		for {
			q, n, good := qualifiedName(toks, text, &i)
			if !good || reserved[strings.ToUpper(n)] && toks[i-1].Kind != sqlscan.QuotedIdent {
				return "", nil, false
			}
			refs = append(refs, colRef{q, n})
			if i < len(toks) && text[toks[i].Start:toks[i].End] == "," {
				i++
				continue
			}
			break
		}
	}
	if peekWord() != "FROM" {
		return "", nil, false
	}
	i++
	schema, name, good := qualifiedName(toks, text, &i)
	if !good || schema != "" && !strings.EqualFold(schema, "main") {
		return "", nil, false
	}
	alias := ""
	if w := peekWord(); w == "AS" {
		i++
		a, good := identAt(toks, text, i)
		if !good {
			return "", nil, false
		}
		alias = a
		i++
	} else if i < len(toks) && (toks[i].Kind == sqlscan.Word && !reserved[w] || toks[i].Kind == sqlscan.QuotedIdent) {
		alias, _ = identAt(toks, text, i)
		i++
	}
	if i < len(toks) {
		switch t := toks[i]; {
		case t.Kind == sqlscan.Semicolon:
		case t.Kind == sqlscan.Word && clauseStart[strings.ToUpper(text[t.Start:t.End])]:
		default:
			return "", nil, false
		}
	}
	depth := 0
	for _, t := range toks[i:] {
		switch raw := text[t.Start:t.End]; {
		case raw == "(":
			depth++
		case raw == ")":
			depth--
		case depth == 0 && t.Kind == sqlscan.Word && compound[strings.ToUpper(raw)]:
			return "", nil, false
		}
	}
	for _, r := range refs {
		if r.qual != "" && !strings.EqualFold(r.qual, name) && !strings.EqualFold(r.qual, alias) {
			return "", nil, false
		}
		cols = append(cols, r.name)
	}
	if star {
		cols = nil
	}
	return name, cols, true
}

// qualifiedName reads "name" or "qual.name" at toks[*i], each part a bare
// word or a quoted identifier, and returns them unquoted.
func qualifiedName(toks []sqlscan.Token, text string, i *int) (qual, name string, ok bool) {
	first, good := identAt(toks, text, *i)
	if !good {
		return "", "", false
	}
	*i++
	if *i+1 < len(toks) && text[toks[*i].Start:toks[*i].End] == "." {
		second, good := identAt(toks, text, *i+1)
		if !good {
			return "", "", false
		}
		*i += 2
		return first, second, true
	}
	return "", first, true
}

// identAt returns the identifier at toks[i]: a bare word or a terminated
// "…", `…` or […] name, unquoted.
func identAt(toks []sqlscan.Token, text string, i int) (string, bool) {
	if i >= len(toks) {
		return "", false
	}
	t := toks[i]
	raw := text[t.Start:t.End]
	switch t.Kind {
	case sqlscan.Word:
		return raw, true
	case sqlscan.QuotedIdent:
		body := raw[1 : len(raw)-1]
		if raw[0] == '[' {
			return body, true
		}
		q := raw[:1]
		return strings.ReplaceAll(body, q+q, q), true
	}
	return "", false
}

// tokens are text's tokens without whitespace and comments.
func tokens(text string) []sqlscan.Token {
	var out []sqlscan.Token
	sqlscan.Scan(sqlscan.SQLite(), text, func(t sqlscan.Token) bool {
		if t.Kind != sqlscan.Space && t.Kind != sqlscan.Comment {
			out = append(out, t)
		}
		return true
	})
	return out
}

var (
	// clauseStart are the keywords that may follow the table in a
	// recognised SELECT.
	clauseStart = map[string]bool{"WHERE": true, "GROUP": true, "ORDER": true, "LIMIT": true}
	compound    = map[string]bool{"UNION": true, "INTERSECT": true, "EXCEPT": true}
	// reserved are keywords that are never a bare column or alias here.
	reserved = map[string]bool{
		"FROM": true, "WHERE": true, "GROUP": true, "ORDER": true, "LIMIT": true, "AS": true, "JOIN": true,
		"NATURAL": true, "LEFT": true, "RIGHT": true, "FULL": true, "INNER": true, "CROSS": true, "OUTER": true,
		"ON": true, "USING": true, "INDEXED": true, "NOT": true, "WINDOW": true, "HAVING": true, "UNION": true,
		"INTERSECT": true, "EXCEPT": true, "VALUES": true, "SELECT": true, "DISTINCT": true, "ALL": true,
		"NULL": true, "CASE": true, "EXISTS": true, "CAST": true,
	}
)
