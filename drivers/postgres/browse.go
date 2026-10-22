package postgres

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

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	keysetPrefix       = "k:"
	maxCursorLen       = 4096
	offsetPrefix       = "o:"
)

var (
	numericLiteral = regexp.MustCompile(`^(?:NaN|[+-]?(?:Infinity|(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?))$`)
	uuidLiteral    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// likeEscaper escapes the LIKE wildcards and the escape character itself.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Browse pages through a table or view with an optional filter and sort. It
// builds one SELECT over the catalog's own spelling of the names and runs it
// through Query, so the read-only transaction, the server-side cursor, the
// single-statement rule and the error mapping all apply to the generated text
// (including Filter.Raw).
//
// A collection with a primary key and no Sort pages by keyset
// (WHERE (pk) > (last pk) ORDER BY pk), which stays cheap and stable on deep
// pages. Otherwise it pages by OFFSET, with the primary key (or ctid) as the
// last sort key so pages do not overlap.
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
		b.where = append(b.where, "("+raw+"\n)")
	}

	keyset := len(schema.PrimaryKey) > 0 && len(opts.Sort) == 0
	pk := make([]string, len(schema.PrimaryKey))
	for i, name := range schema.PrimaryKey {
		pk[i] = pgx.Identifier{name}.Sanitize()
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
		if k.Desc {
			order = append(order, col+" DESC NULLS FIRST")
		} else {
			order = append(order, col+" ASC NULLS LAST")
		}
	}
	switch { // tiebreak so paging is stable
	case len(pk) > 0:
		order = append(order, pk...)
	case schema.Type != driver.CollectionView:
		order = append(order, "ctid")
	}

	var sb strings.Builder
	sb.WriteString("SELECT * FROM " + pgx.Identifier{schema.Ref.Namespace, schema.Ref.Name}.Sanitize())
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

	rs, err := c.Query(ctx, driver.Query{Text: sb.String(), Params: b.params, Limit: limit, ReadOnly: opts.ReadOnly})
	if err != nil {
		return nil, err
	}
	p := &pageResultSet{ResultSet: rs, next: offset + limit}
	if keyset {
		p.keyIdx = make([]int, len(schema.PrimaryKey))
		cols := rs.Columns()
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
	return "$" + strconv.Itoa(len(b.params))
}

// field resolves a caller's field name to the quoted column. The exact name
// wins (quoted identifiers are case-sensitive); otherwise a unique
// case-insensitive match is accepted. Unknown or ambiguous → ErrNotFound.
func (b *browseQuery) field(name string) (string, error) {
	var folded []string
	for _, f := range b.schema.Fields {
		if f.Name == name {
			return pgx.Identifier{f.Name}.Sanitize(), nil
		}
		if strings.EqualFold(f.Name, name) {
			folded = append(folded, f.Name)
		}
	}
	switch len(folded) {
	case 1:
		return pgx.Identifier{folded[0]}.Sanitize(), nil
	case 0:
		return "", driver.Wrap(driver.ErrNotFound, Name, b.op, fmt.Errorf("no field %q in %s", name, b.schema.Ref.Name))
	}
	return "", driver.Wrap(driver.ErrNotFound, Name, b.op, fmt.Errorf("field %q is ambiguous in %s (%s)", name, b.schema.Ref.Name, strings.Join(folded, ", ")))
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
		// The placeholder is cast so the concatenation resolves to text.
		ph := "CAST(" + b.bind(driver.String(likeEscaper.Replace(val.String()))) + " AS text)"
		if cond.Op == driver.OpContains {
			clause = "CAST(" + col + " AS text) LIKE '%' || " + ph + ` || '%' ESCAPE '\'`
		} else {
			clause = "CAST(" + col + " AS text) LIKE " + ph + ` || '%' ESCAPE '\'`
		}
	case driver.OpIn:
		list, ok := val.AsList()
		if !ok {
			return driver.Wrap(driver.ErrSyntax, Name, b.op, errors.New("in needs a list value"))
		}
		for _, v := range list {
			if err := b.scalar(v); err != nil {
				return err
			}
		}
		clause = col + " = ANY(" + b.bind(val) + ")"
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
	if len(pk) == 1 {
		b.where = append(b.where, pk[0]+" > "+ph[0])
	} else {
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
		typ, native := driver.TypeUnknown, ""
		for _, f := range schema.Fields {
			if f.Name == name {
				typ, native = f.Type, f.NativeType
			}
		}
		v, err := keyValue(typ, native, parts[i])
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: key %q: %w", name, err)
		}
		out[i] = v
	}
	return out, nil
}

// keyValue rebuilds a Value from its JSON form, given the column's type.
func keyValue(typ driver.Type, native string, x any) (driver.Value, error) {
	if x == nil {
		return driver.Value{}, errors.New("null key")
	}
	switch typ {
	case driver.TypeInt:
		if n, ok := x.(json.Number); ok {
			i, err := n.Int64()
			return driver.Int(i), err
		}
	case driver.TypeFloat:
		switch t := x.(type) {
		case json.Number:
			f, err := t.Float64()
			return driver.Float(f), err
		case string: // "NaN", "+Inf", "-Inf"
			f, err := strconv.ParseFloat(t, 64)
			return driver.Float(f), err
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
	case driver.TypeJSON:
		raw, err := json.Marshal(x)
		return driver.JSON(raw), err
	default: // strings, uuids, enums and the rest travel as text
		switch t := x.(type) {
		case string:
			if native == "uuid" && !uuidLiteral.MatchString(t) {
				return driver.Value{}, fmt.Errorf("%q is not a uuid", t)
			}
			return driver.String(t), nil
		case json.Number:
			return driver.String(t.String()), nil
		case bool:
			return driver.String(strconv.FormatBool(t)), nil
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
	if ok && p.keyIdx != nil {
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
