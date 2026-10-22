package mssql

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

// likeEscaper escapes the LIKE wildcards (SQL Server's include [ … ]) and
// the escape character '!'.
var likeEscaper = strings.NewReplacer(`!`, `!!`, `%`, `!%`, `_`, `!_`, `[`, `![`)

// unorderable are the types SQL Server cannot sort or compare.
var unorderable = map[string]bool{"text": true, "ntext": true, "image": true, "xml": true, "geography": true, "geometry": true}

// Browse pages through a table or view with an optional filter and sort. It
// builds one SELECT over the catalog's own spelling of the names (quoted
// with brackets, three-part) and runs it through the same path as Query, so
// on a read-only call the allow-list, the rolled-back transaction and its
// post-check apply to the generated text, Filter.Raw included.
//
// A collection with a primary key and no Sort pages by keyset (SELECT TOP
// … WHERE key > last key ORDER BY key), which stays cheap and stable on deep
// pages. Otherwise it pages by OFFSET … FETCH, with the primary key (or,
// without one, every sortable column) as the last sort keys so pages do not
// overlap. NULLs sort last ascending and first descending, as on the other
// SQL drivers.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	t, err := c.describeIn(ctx, c.db, op, ref)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	schema := t.schema
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
	raw := opts.Filter.Raw
	if strings.TrimSpace(raw) != "" {
		if err := checkRawFilter(raw); err != nil {
			return nil, err
		}
		// The newline ends a trailing "--" comment in raw.
		b.where = append(b.where, "("+raw+"\n)")
	}

	keyset := len(schema.PrimaryKey) > 0 && len(opts.Sort) == 0 && !slices.ContainsFunc(schema.PrimaryKey, func(name string) bool {
		f := schemaField(schema, name)
		// A FLOAT key is not keyset-paged: comparing it with the cursor's
		// double can repeat rows.
		return f == nil || f.Type == driver.TypeFloat || unorderable[baseOf(f.NativeType)]
	})
	offset := 0
	if keyset {
		if err := b.keysetWhere(opts.Cursor); err != nil {
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
		if k.Desc {
			order = append(order, "CASE WHEN "+col+" IS NULL THEN 0 ELSE 1 END", col+" DESC")
		} else {
			order = append(order, "CASE WHEN "+col+" IS NULL THEN 1 ELSE 0 END", col+" ASC")
		}
	}
	if len(schema.PrimaryKey) > 0 { // tiebreak so paging is stable
		for _, name := range schema.PrimaryKey {
			order = append(order, quoteIdent(name))
		}
	} else {
		for _, f := range schema.Fields {
			if !unorderable[baseOf(f.NativeType)] {
				order = append(order, quoteIdent(f.Name))
			}
		}
	}
	if len(order) == 0 {
		order = []string{"(SELECT NULL)"}
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	if keyset {
		fmt.Fprintf(&sb, "TOP (%d) ", limit+1)
	}
	sb.WriteString("* FROM " + t.name())
	if len(b.where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(b.where, " AND "))
	}
	sb.WriteString(" ORDER BY " + strings.Join(order, ", "))
	if !keyset {
		fmt.Fprintf(&sb, " OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", offset, limit+1)
	}

	check := checker(checkReadOnly)
	if strings.TrimSpace(raw) != "" {
		check = func(text string) error {
			if err := checkRawFilter(raw); err != nil {
				return err
			}
			return checkReadOnly(text)
		}
	}
	rs, err := c.run(ctx, driver.Query{Text: sb.String(), Params: b.params, Limit: limit}, c.readOnly || opts.ReadOnly, check)
	if err != nil {
		return nil, err
	}
	p := &pageResultSet{ResultSet: rs, next: offset + limit}
	if keyset {
		cols := rs.Columns()
		p.keyIdx = make([]int, len(schema.PrimaryKey))
		for i, name := range schema.PrimaryKey {
			p.keyIdx[i] = slices.IndexFunc(cols, func(col driver.Column) bool { return col.Name == name })
			if p.keyIdx[i] < 0 { // cannot happen with SELECT *; fail rather than page wrongly
				_ = rs.Close()
				return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("primary key column %q missing from result", name))
			}
		}
	}
	return p, nil
}

// baseOf is the type name of a native type: "nvarchar" for "nvarchar(255)".
func baseOf(native string) string {
	if i := strings.IndexByte(native, '('); i >= 0 {
		native = native[:i]
	}
	return strings.ToLower(native)
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
	return "@p" + strconv.Itoa(len(b.params))
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
			clause = "CAST(" + col + " AS nvarchar(max)) LIKE N'%' + " + ph + " + N'%' ESCAPE '!'"
		} else {
			clause = "CAST(" + col + " AS nvarchar(max)) LIKE " + ph + " + N'%' ESCAPE '!'"
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
// cursor. SQL Server has no row-value comparison, so a composite key
// expands to (a > @1) OR (a = @1 AND b > @2) …
func (b *browseQuery) keysetWhere(cursor string) error {
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
	pk := b.schema.PrimaryKey
	var ors []string
	for i := range pk {
		var ands []string
		for j := 0; j < i; j++ {
			ands = append(ands, quoteIdent(pk[j])+" = "+ph[j])
		}
		ands = append(ands, quoteIdent(pk[i])+" > "+ph[i])
		ors = append(ors, "("+strings.Join(ands, " AND ")+")")
	}
	b.where = append(b.where, "("+strings.Join(ors, " OR ")+")")
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
			i, err := n.Int64()
			return driver.Int(i), err
		}
	case driver.TypeBool:
		if b, ok := x.(bool); ok {
			return driver.Bool(b), nil
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
	default: // strings, GUIDs, TIME and the rest travel as text
		switch t := x.(type) {
		case string:
			return driver.String(t), nil
		case json.Number:
			return driver.String(t.String()), nil
		}
	}
	return driver.Value{}, fmt.Errorf("unexpected %T for a %s column", x, typ)
}

// pageResultSet adds the next-page cursor to the inner result's stats. It
// remembers the primary key of the last row it returned, for keyset paging.
type pageResultSet struct {
	driver.ResultSet
	next   int   // offset of the next page (offset paging)
	keyIdx []int // primary key column positions (keyset paging); nil for offset paging
	last   []driver.Value
}

func (p *pageResultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	row, ok, err := p.ResultSet.Next(ctx)
	if !ok {
		return row, ok, err
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
