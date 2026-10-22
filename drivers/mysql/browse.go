package mysql

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

// likeEscaper escapes the LIKE wildcards and the escape character. '!' is
// the escape character rather than '\', whose meaning in a string literal
// depends on the session's NO_BACKSLASH_ESCAPES.
var likeEscaper = strings.NewReplacer(`!`, `!!`, `%`, `!%`, `_`, `!_`)

// Browse pages through a table or view with an optional filter and sort. It
// builds one SELECT over the catalog's own spelling of the names (quoted with
// backticks) and runs it through the same path as Query, so on a read-only
// call the allow-list, the read-only transaction and its post-check apply to
// the generated text, Filter.Raw included. Like any query, it runs on a
// dedicated connection that is discarded afterwards.
//
// A collection with a primary key and no Sort pages by keyset
// (WHERE (pk) > (last pk) ORDER BY pk), which stays cheap and stable on deep
// pages. Otherwise it pages by OFFSET, with the primary key (or, without one,
// every column) as the last sort keys so pages do not overlap.
//
// Column types come from Describe, so a TINYINT(1) column is Bool: its 0 and
// 1 values are returned as false and true (other values stay Int).
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (_ driver.ResultSet, err error) {
	defer func() { err = c.named(err) }()
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
		return nil, err
	}
	b := &browseQuery{op: op, schema: schema, expandKeyset: c.syntaxFlavor() == flavorSingleStore}
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
		mode := lexMode{}
		if strings.ContainsRune(raw, '\\') { // only a backslash makes sql_mode matter
			if mode, err = c.sessionLexMode(ctx); err != nil {
				return nil, err
			}
		}
		if err := checkRawFilter(raw, mode); err != nil {
			return nil, err
		}
		// The newline ends a trailing "#" or "-- " comment in raw.
		b.where = append(b.where, "("+raw+"\n)")
	}

	// A FLOAT key is not keyset-paged: the column compared with the double
	// the cursor holds can be greater than itself, repeating rows.
	keyset := len(schema.PrimaryKey) > 0 && len(opts.Sort) == 0 && !slices.ContainsFunc(schema.PrimaryKey, func(name string) bool {
		f := schemaField(schema, name)
		return f == nil || f.Type == driver.TypeFloat
	})
	pk := make([]string, len(schema.PrimaryKey))
	for i, name := range schema.PrimaryKey {
		pk[i] = quoteIdent(name)
	}
	offset := 0
	if keyset {
		if err := b.keysetWhere(opts.Cursor, pk); err != nil {
			return nil, err
		}
	} else if offset, err = parseOffsetCursor(opts.Cursor); err != nil {
		return nil, err
	}

	var order []string
	for _, k := range opts.Sort {
		col, err := b.field(k.Field)
		if err != nil {
			return nil, err
		}
		// MySQL has no NULLS FIRST/LAST: "col IS NULL" sorts the NULLs
		// last ascending and first descending, as on the other SQL drivers.
		if k.Desc {
			order = append(order, col+" IS NULL DESC", col+" DESC")
		} else {
			order = append(order, col+" IS NULL", col+" ASC")
		}
	}
	if len(pk) > 0 { // tiebreak so paging is stable
		order = append(order, pk...)
	} else {
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
	fmt.Fprintf(&sb, " LIMIT %d", limit+1)
	if !keyset {
		fmt.Fprintf(&sb, " OFFSET %d", offset)
	}

	check := checker(checkReadOnly)
	if raw := opts.Filter.Raw; strings.TrimSpace(raw) != "" {
		// On a read-only call the guard re-checks under the browse
		// connection's own sql_mode; the raw filter's shape is re-checked
		// with it.
		check = func(text string, mode lexMode) error {
			if err := checkRawFilter(raw, mode); err != nil {
				return err
			}
			return checkReadOnly(text, mode)
		}
	}
	rs, err := c.run(ctx, driver.Query{Text: sb.String(), Params: b.params, Limit: limit}, c.readOnly || opts.ReadOnly, check)
	if err != nil {
		return nil, err
	}
	p := &pageResultSet{ResultSet: rs, next: offset + limit, cols: slices.Clone(rs.Columns())}
	for i, col := range p.cols {
		if f := schemaField(schema, col.Name); f != nil && f.Type == driver.TypeBool && col.Type == driver.TypeInt {
			p.cols[i].Type = driver.TypeBool
			p.boolIdx = append(p.boolIdx, i)
		}
	}
	if keyset {
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
	// expandKeyset spells a composite keyset comparison as ORs of ANDs, for
	// SingleStore, which has no row-constructor comparison.
	expandKeyset bool
}

// bind adds a parameter and returns its placeholder.
func (b *browseQuery) bind(v driver.Value) string {
	b.params = append(b.params, v)
	return "?"
}

// field resolves a caller's field name to the quoted column. The exact name
// wins; otherwise a unique case-insensitive match is accepted. Unknown or
// ambiguous → ErrNotFound.
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
			clause = "CAST(" + col + " AS CHAR) LIKE CONCAT('%', " + ph + ", '%') ESCAPE '!'"
		} else {
			clause = "CAST(" + col + " AS CHAR) LIKE CONCAT(" + ph + ", '%') ESCAPE '!'"
		}
	case driver.OpIn:
		list, ok := val.AsList()
		if !ok {
			return driver.Wrap(driver.ErrSyntax, Name, b.op, errors.New("in needs a list value"))
		}
		if len(list) == 0 {
			clause = "FALSE"
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

// keysetWhere adds the "after the cursor's key" clause, when there is a cursor.
func (b *browseQuery) keysetWhere(cursor string, pk []string) error {
	if cursor == "" {
		return nil
	}
	vals, err := decodeKeysetCursor(cursor, b.schema)
	if err != nil {
		return driver.Wrap(driver.ErrSyntax, Name, b.op, err)
	}
	ph := make([]string, len(vals))
	for i, v := range vals {
		ph[i] = b.bind(v)
	}
	switch {
	case len(pk) == 1:
		b.where = append(b.where, pk[0]+" > "+ph[0])
	case b.expandKeyset:
		// (a > ? OR (a = ? AND b > ?) OR …), binding the key again per term.
		b.params = b.params[:len(b.params)-len(vals)]
		terms := make([]string, len(pk))
		for i := range pk {
			parts := make([]string, 0, i+1)
			for j := range i {
				parts = append(parts, pk[j]+" = "+b.bind(vals[j]))
			}
			parts = append(parts, pk[i]+" > "+b.bind(vals[i]))
			if i == 0 {
				terms[i] = parts[0]
			} else {
				terms[i] = "(" + strings.Join(parts, " AND ") + ")"
			}
		}
		b.where = append(b.where, "("+strings.Join(terms, " OR ")+")")
	default:
		b.where = append(b.where, "("+strings.Join(pk, ", ")+") > ("+strings.Join(ph, ", ")+")")
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
			if !numericLiteral.MatchString(n.String()) { // BIGINT UNSIGNED above MaxInt64
				return driver.Value{}, fmt.Errorf("%q is not a number", n)
			}
			return driver.Decimal(n.String()), nil
		}
		if s, ok := x.(string); ok && numericLiteral.MatchString(s) { // a Decimal-valued BIGINT UNSIGNED
			return driver.Decimal(s), nil
		}
	case driver.TypeFloat:
		if n, ok := x.(json.Number); ok {
			f, err := n.Float64()
			return driver.Float(f), err
		}
	case driver.TypeBool:
		switch t := x.(type) {
		case bool:
			return driver.Bool(t), nil
		case json.Number: // a TINYINT(1) value other than 0 or 1
			i, err := t.Int64()
			return driver.Int(i), err
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
	case driver.TypeJSON:
		raw, err := json.Marshal(x)
		return driver.JSON(raw), err
	default: // strings, enums and the rest travel as text
		switch t := x.(type) {
		case string:
			return driver.String(t), nil
		case json.Number:
			return driver.String(t.String()), nil
		case bool:
			return driver.String(strconv.FormatBool(t)), nil
		}
	}
	return driver.Value{}, fmt.Errorf("unexpected %T for a %s column", x, typ)
}

// pageResultSet adds the next-page cursor to the inner result's stats and
// turns TINYINT(1) values into Bools. It remembers the primary key of the
// last row it returned, for keyset paging.
type pageResultSet struct {
	driver.ResultSet
	cols    []driver.Column
	boolIdx []int // TINYINT(1) column positions
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
