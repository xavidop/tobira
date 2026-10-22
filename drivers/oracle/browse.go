package oracle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	keysetPrefix       = "k:"
	maxCursorLen       = 4096
	offsetPrefix       = "o:"
)

var numericLiteral = regexp.MustCompile(`^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$`)

// likeEscaper escapes the LIKE wildcards and the escape character '!'.
var likeEscaper = strings.NewReplacer(`!`, `!!`, `%`, `!%`, `_`, `!_`)

// Browse pages through a table or view with an optional filter and sort. It
// builds one SELECT over the dictionary's spelling of the names (quoted) and
// runs it through the same path as Query, so on a read-only call the
// allow-list, the read-only transaction and its check apply to the
// generated text, Filter.Raw included.
//
// A collection with a primary key and no Sort pages by keyset (WHERE key
// after the last key, ORDER BY key, FETCH FIRST), which stays cheap and
// stable on deep pages. Otherwise it pages with OFFSET … FETCH NEXT, with the
// primary key (or ROWID for a table without one, or every column of a view)
// as the last sort keys so pages do not overlap. NULLs sort last ascending
// and first descending.
//
// Column types come from Describe, so BOOLEAN columns (which reach go-ora as
// NUMBER) are Bool.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	schema, err := c.Describe(ctx, ref)
	if err != nil {
		var de *driver.Error
		if errors.As(err, &de) {
			de.Op = op
		}
		return nil, err
	}
	b, err := buildBrowse(schema, opts, limit)
	if err != nil {
		return nil, err
	}
	rs, err := c.run(ctx, driver.Query{Text: b.text, Params: b.params, Limit: limit}, c.readOnly || opts.ReadOnly, checkReadOnly)
	if err != nil {
		return nil, err
	}
	p := &pageResultSet{ResultSet: rs, next: b.offset + limit, cols: slices.Clone(rs.Columns())}
	for i, col := range p.cols {
		if f := schemaField(schema, col.Name); f != nil {
			p.cols[i].Source = &schema.Ref
			p.cols[i].Key = slices.Contains(schema.PrimaryKey, f.Name)
			if f.Type == driver.TypeBool && col.Type != driver.TypeBool {
				p.cols[i].Type = driver.TypeBool
				p.boolIdx = append(p.boolIdx, i)
			}
		}
	}
	if b.keyset {
		p.keyIdx = make([]int, len(schema.PrimaryKey))
		for i, name := range schema.PrimaryKey {
			p.keyIdx[i] = slices.IndexFunc(p.cols, func(col driver.Column) bool { return col.Name == name })
			if p.keyIdx[i] < 0 { // cannot happen with SELECT *; fail rather than page wrongly
				_ = rs.Close()
				return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("primary key column %q missing from result", name))
			}
		}
	}
	return p, nil
}

// browseSQL is a built Browse statement.
type browseSQL struct {
	text   string
	params []driver.Value
	keyset bool
	offset int
}

// buildBrowse builds the SELECT of one Browse page, reading limit+1 rows so
// the result knows whether a next page exists.
func buildBrowse(schema *driver.CollectionSchema, opts driver.BrowseOptions, limit int) (*browseSQL, error) {
	const op = "browse"
	b := &browseQuery{op: op, schema: schema}
	for _, cond := range opts.Filter.Conditions {
		col, err := b.field(cond.Field)
		if err != nil {
			return nil, err
		}
		if err := b.addCondition(col, cond); err != nil {
			return nil, err
		}
	}
	if raw := opts.Filter.Raw; strings.TrimSpace(raw) != "" {
		if err := checkRawFilter(raw); err != nil {
			return nil, err
		}
		// The newline ends a trailing "--" comment in raw.
		b.where = append(b.where, "("+raw+"\n)")
	}

	// A float key is not keyset-paged: comparing with the value the cursor
	// holds could repeat or skip rows.
	keyset := len(schema.PrimaryKey) > 0 && len(opts.Sort) == 0 && !slices.ContainsFunc(schema.PrimaryKey, func(name string) bool {
		f := schemaField(schema, name)
		return f == nil || f.Type == driver.TypeFloat
	})
	pk := make([]string, len(schema.PrimaryKey))
	for i, name := range schema.PrimaryKey {
		pk[i] = quoteIdent(name)
	}
	out := &browseSQL{keyset: keyset}
	if keyset {
		if err := b.keysetWhere(opts.Cursor, pk); err != nil {
			return nil, err
		}
	} else {
		n, err := parseOffsetCursor(opts.Cursor)
		if err != nil {
			return nil, err
		}
		out.offset = n
	}

	var order []string
	for _, k := range opts.Sort {
		col, err := b.field(k.Field)
		if err != nil {
			return nil, err
		}
		if k.Desc {
			order = append(order, col+" DESC NULLS FIRST")
		} else {
			order = append(order, col+" ASC NULLS LAST")
		}
	}
	switch {
	case len(pk) > 0: // tiebreak so paging is stable
		order = append(order, pk...)
	case schema.Type == driver.CollectionTable:
		order = append(order, "ROWID")
	default:
		for _, f := range schema.Fields {
			order = append(order, quoteIdent(f.Name))
		}
	}

	var sb strings.Builder
	sb.WriteString("SELECT * FROM " + quoteIdent(schema.Ref.Namespace) + "." + quoteIdent(schema.Ref.Name))
	if len(b.where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(b.where, " AND "))
	}
	if len(order) > 0 {
		sb.WriteString(" ORDER BY " + strings.Join(order, ", "))
	}
	if keyset {
		fmt.Fprintf(&sb, " FETCH FIRST %d ROWS ONLY", limit+1)
	} else {
		fmt.Fprintf(&sb, " OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", out.offset, limit+1)
	}
	out.text = sb.String()
	out.params = b.params
	return out, nil
}

func schemaField(schema *driver.CollectionSchema, name string) *driver.Field {
	for i := range schema.Fields {
		if schema.Fields[i].Name == name {
			return &schema.Fields[i]
		}
	}
	return nil
}

// browseQuery accumulates the WHERE clauses and their parameters.
type browseQuery struct {
	op     string
	schema *driver.CollectionSchema
	where  []string
	params []driver.Value
}

// bind adds a parameter and returns its placeholder.
func (b *browseQuery) bind(v driver.Value) string {
	b.params = append(b.params, v)
	return ":" + strconv.Itoa(len(b.params))
}

// field resolves a caller's field name to the quoted column.
func (b *browseQuery) field(name string) (string, error) {
	n, err := field(b.op, b.schema, name)
	if err != nil {
		return "", err
	}
	return quoteIdent(n), nil
}

// addCondition renders one condition against the quoted column col.
func (b *browseQuery) addCondition(col string, cond driver.Condition) error {
	val := cond.Value
	cmp := map[driver.Op]string{
		driver.OpEq: "=", driver.OpNe: "<>", driver.OpLt: "<", driver.OpLe: "<=", driver.OpGt: ">", driver.OpGe: ">=",
	}
	var clause string
	switch cond.Op {
	case driver.OpIsNull:
		clause = col + " IS NULL"
	case driver.OpNotNull:
		clause = col + " IS NOT NULL"
	case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		if val.IsNull() && cond.Op == driver.OpEq {
			clause = col + " IS NULL"
			break
		}
		if val.IsNull() && cond.Op == driver.OpNe {
			clause = col + " IS NOT NULL"
			break
		}
		if err := b.scalar(val); err != nil {
			return err
		}
		clause = col + " " + cmp[cond.Op] + " " + b.bind(val)
	case driver.OpContains, driver.OpPrefix:
		if err := b.scalar(val); err != nil {
			return err
		}
		ph := b.bind(driver.String(likeEscaper.Replace(val.String())))
		if cond.Op == driver.OpContains {
			clause = "TO_CHAR(" + col + ") LIKE '%' || " + ph + " || '%' ESCAPE '!'"
		} else {
			clause = "TO_CHAR(" + col + ") LIKE " + ph + " || '%' ESCAPE '!'"
		}
	case driver.OpIn:
		list, ok := val.AsList()
		if !ok {
			return driver.Wrap(driver.ErrSyntax, Name, b.op, errors.New("in needs a list value"))
		}
		if len(list) == 0 {
			clause = "1 = 0"
			break
		}
		marks := make([]string, len(list))
		for i, v := range list {
			if err := b.scalar(v); err != nil {
				return err
			}
			marks[i] = b.bind(v)
		}
		clause = col + " IN (" + strings.Join(marks, ", ") + ")"
	default:
		return driver.Wrap(driver.ErrUnsupported, Name, b.op, fmt.Errorf("unknown operator %q", cond.Op))
	}
	b.where = append(b.where, clause)
	return nil
}

// scalar rejects values that are not a single comparable value.
func (b *browseQuery) scalar(v driver.Value) error {
	switch v.Type() {
	case driver.TypeList, driver.TypeObject:
		return driver.Wrap(driver.ErrSyntax, Name, b.op, fmt.Errorf("a %s value is not valid here", v.Type()))
	}
	if _, err := toArg(v); err != nil {
		return driver.Wrap(driver.ErrUnsupported, Name, b.op, err)
	}
	return nil
}

// keysetWhere adds the "after the cursor's key" clause, when there is a
// cursor. Oracle has no row-value comparison, so a composite key expands
// to (a > x) OR (a = x AND b > y) ….
func (b *browseQuery) keysetWhere(cursor string, pk []string) error {
	if cursor == "" {
		return nil
	}
	vals, err := decodeKeysetCursor(cursor, b.schema)
	if err != nil {
		return driver.Wrap(driver.ErrSyntax, Name, b.op, err)
	}
	var ors []string
	for i := range pk {
		var ands []string
		for j := 0; j < i; j++ {
			ands = append(ands, pk[j]+" = "+b.bind(vals[j]))
		}
		ands = append(ands, pk[i]+" > "+b.bind(vals[i]))
		ors = append(ors, "("+strings.Join(ands, " AND ")+")")
	}
	if len(ors) == 1 {
		b.where = append(b.where, ors[0])
	} else {
		b.where = append(b.where, "("+strings.Join(ors, " OR ")+")")
	}
	return nil
}

func parseOffsetCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > maxCursorLen {
		return 0, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor: too long"))
	}
	n, err := strconv.Atoi(strings.TrimPrefix(cursor, offsetPrefix))
	if !strings.HasPrefix(cursor, offsetPrefix) || err != nil || n < 0 {
		return 0, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("invalid cursor %q", cursor))
	}
	return n, nil
}

// encodeKeysetCursor encodes the primary key of the last row of a page.
func encodeKeysetCursor(key []driver.Value) (string, error) {
	raw, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	return keysetPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeKeysetCursor parses a keyset cursor into one Value per primary key
// column, typed after the column so the server compares like with like.
func decodeKeysetCursor(cursor string, schema *driver.CollectionSchema) ([]driver.Value, error) {
	if len(cursor) > maxCursorLen {
		return nil, fmt.Errorf("invalid cursor: longer than %d bytes", maxCursorLen)
	}
	enc, ok := strings.CutPrefix(cursor, keysetPrefix)
	if !ok {
		return nil, fmt.Errorf("invalid cursor %q", cursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parts []any
	if err := dec.Decode(&parts); err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	if len(parts) != len(schema.PrimaryKey) {
		return nil, fmt.Errorf("invalid cursor: has %d key values, the primary key has %d columns", len(parts), len(schema.PrimaryKey))
	}
	out := make([]driver.Value, len(parts))
	for i, name := range schema.PrimaryKey {
		typ := driver.TypeUnknown
		if f := schemaField(schema, name); f != nil {
			typ = f.Type
		}
		v, err := keyValue(typ, parts[i])
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: key %q: %w", name, err)
		}
		out[i] = v
	}
	return out, nil
}

// keyValue rebuilds a Value from its JSON form, given the column's type.
func keyValue(typ driver.Type, x any) (driver.Value, error) {
	if x == nil {
		return driver.Value{}, errors.New("null key")
	}
	switch typ {
	case driver.TypeInt:
		if n, ok := x.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				return driver.Int(i), nil
			}
		}
	case driver.TypeDecimal:
		switch t := x.(type) {
		case json.Number:
			return driver.Decimal(t.String()), nil
		case string:
			if !numericLiteral.MatchString(t) {
				return driver.Value{}, fmt.Errorf("%q is not a number", t)
			}
			return driver.Decimal(t), nil
		}
	case driver.TypeBool:
		switch t := x.(type) {
		case bool:
			return driver.Bool(t), nil
		case json.Number:
			i, err := t.Int64()
			return driver.Int(i), err
		}
	case driver.TypeTime:
		if s, ok := x.(string); ok {
			t, err := time.Parse(time.RFC3339Nano, s)
			return driver.Time(t), err
		}
	case driver.TypeBytes:
		if s, ok := x.(string); ok {
			b, err := base64.StdEncoding.DecodeString(s)
			return driver.Bytes(b), err
		}
	default: // strings and the rest travel as text
		switch t := x.(type) {
		case string:
			return driver.String(t), nil
		case json.Number:
			if typ == driver.TypeUnknown {
				return driver.String(t.String()), nil
			}
		}
	}
	return driver.Value{}, fmt.Errorf("unexpected %T for a %s column", x, typ)
}

// pageResultSet adds the next-page cursor to the inner result's stats and
// turns BOOLEAN columns' 1 and 0 into Bools. It remembers the primary key of
// the last row it returned, for keyset paging.
type pageResultSet struct {
	driver.ResultSet
	cols    []driver.Column
	boolIdx []int
	next    int   // offset of the next page (offset paging)
	keyIdx  []int // primary key column positions (keyset paging); nil for offset paging
	last    []driver.Value
}

func (p *pageResultSet) Columns() []driver.Column { return p.cols }

func (p *pageResultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	row, ok, err := p.ResultSet.Next(ctx)
	if !ok {
		return row, ok, err
	}
	for _, i := range p.boolIdx {
		if n, isInt := row[i].AsInt(); isInt && row[i].Type() == driver.TypeInt && (n == 0 || n == 1) {
			row[i] = driver.Bool(n == 1)
		}
	}
	if p.keyIdx != nil {
		p.last = p.last[:0]
		for _, i := range p.keyIdx {
			p.last = append(p.last, row[i])
		}
	}
	return row, ok, err
}

func (p *pageResultSet) Stats() driver.Stats {
	st := p.ResultSet.Stats()
	if !st.Truncated {
		return st
	}
	if p.keyIdx == nil {
		st.NextCursor = offsetPrefix + strconv.Itoa(p.next)
		return st
	}
	if len(p.last) > 0 {
		if cur, err := encodeKeysetCursor(p.last); err == nil {
			st.NextCursor = cur
		}
	}
	return st
}
