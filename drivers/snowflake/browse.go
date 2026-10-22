package snowflake

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	maxCursorLen       = 64
	offsetPrefix       = "o:"
)

var _ driver.Browser = (*conn)(nil)

// Browse pages through a table or view with an optional filter and sort. It
// builds one SELECT over the catalog's own spelling of the names (quoted)
// and runs it through the same path as Query, so on a read-only call the
// allow-list applies to the generated text, Filter.Raw included.
//
// Pages are LIMIT/OFFSET, ordered by the caller's Sort (NULLs last
// ascending, first descending) and then by the primary key, or, for a
// table without one, by every column that can be ordered. Snowflake does
// not enforce primary keys, so rows tied on a duplicated key may move
// between pages if the table changes while it is browsed.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	offset, err := parseOffsetCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	raw := opts.Filter.Raw
	if strings.TrimSpace(raw) != "" {
		if err := checkRawFilter(raw); err != nil {
			return nil, err
		}
	}
	schema, err := c.describe(ctx, op, ref)
	if err != nil {
		return nil, c.scrub(err)
	}
	b := &browseQuery{schema: schema}
	for _, cond := range opts.Filter.Conditions {
		col, err := fieldColumn(op, schema, cond.Field)
		if err != nil {
			return nil, err
		}
		if err := b.addCondition(col, cond); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(raw) != "" {
		// The newline ends a trailing line comment in raw.
		b.where = append(b.where, "("+raw+"\n)")
	}

	var order []string
	for _, k := range opts.Sort {
		col, err := fieldColumn(op, schema, k.Field)
		if err != nil {
			return nil, err
		}
		if k.Desc {
			order = append(order, col+" DESC NULLS FIRST")
		} else {
			order = append(order, col+" ASC NULLS LAST")
		}
	}
	if len(schema.PrimaryKey) > 0 {
		for _, k := range schema.PrimaryKey {
			order = append(order, quoteIdent(k))
		}
	} else {
		for _, f := range schema.Fields {
			if orderable(f.Type) {
				order = append(order, quoteIdent(f.Name))
			}
		}
	}

	db, sch, _ := splitNamespace(schema.Ref.Namespace)
	var sb strings.Builder
	sb.WriteString("SELECT * FROM " + quoteIdent(db) + "." + quoteIdent(sch) + "." + quoteIdent(schema.Ref.Name))
	if len(b.where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(b.where, " AND "))
	}
	if len(order) > 0 {
		sb.WriteString(" ORDER BY " + strings.Join(order, ", "))
	}
	fmt.Fprintf(&sb, " LIMIT %d OFFSET %d", limit+1, offset)
	rs, err := c.run(ctx, op, driver.Query{Text: sb.String(), Params: b.params, Limit: limit}, c.readOnly || opts.ReadOnly)
	if err != nil {
		return nil, err
	}
	return &pageResultSet{ResultSet: rs, next: offset + limit}, nil
}

// orderable reports whether a column of type t can be an ORDER BY key.
func orderable(t driver.Type) bool {
	return t != driver.TypeJSON && t != driver.TypeUnknown && t != driver.TypeNull && t != driver.TypeVector
}

// fieldColumn resolves a caller's field name to the quoted column. The
// exact name wins; otherwise a unique case-insensitive match is accepted.
// Unknown or ambiguous → ErrNotFound.
func fieldColumn(op string, schema *driver.CollectionSchema, name string) (string, error) {
	f, err := findField(op, schema, name)
	if err != nil {
		return "", err
	}
	return quoteIdent(f.Name), nil
}

func findField(op string, schema *driver.CollectionSchema, name string) (driver.Field, error) {
	var folded []driver.Field
	for _, f := range schema.Fields {
		if f.Name == name {
			return f, nil
		}
		if strings.EqualFold(f.Name, name) {
			folded = append(folded, f)
		}
	}
	switch len(folded) {
	case 1:
		return folded[0], nil
	case 0:
		return driver.Field{}, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no field %q in %s", name, schema.Ref.Name))
	}
	return driver.Field{}, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("field %q is ambiguous in %s", name, schema.Ref.Name))
}

// browseQuery accumulates the WHERE clauses and their parameters.
type browseQuery struct {
	schema *driver.CollectionSchema
	where  []string
	params []driver.Value
}

// bind adds a parameter and returns its placeholder.
func (b *browseQuery) bind(v driver.Value) string {
	b.params = append(b.params, v)
	return "?"
}

// addCondition renders one condition against the quoted column col.
func (b *browseQuery) addCondition(col string, cond driver.Condition) error {
	val := cond.Value
	cmp := map[driver.Op]string{
		driver.OpEq: "=", driver.OpNe: "!=", driver.OpLt: "<", driver.OpLe: "<=", driver.OpGt: ">", driver.OpGe: ">=",
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
		if err := scalar(val); err != nil {
			return err
		}
		clause = col + " " + cmp[cond.Op] + " " + b.bind(val)
	case driver.OpContains, driver.OpPrefix:
		if err := scalar(val); err != nil {
			return err
		}
		ph := b.bind(driver.String(val.String()))
		if cond.Op == driver.OpContains {
			clause = "CONTAINS(TO_VARCHAR(" + col + "), " + ph + ")"
		} else {
			clause = "STARTSWITH(TO_VARCHAR(" + col + "), " + ph + ")"
		}
	case driver.OpIn:
		list, ok := val.AsList()
		if !ok {
			return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("in needs a list value"))
		}
		if len(list) == 0 {
			clause = "FALSE"
			break
		}
		marks := make([]string, len(list))
		for i, v := range list {
			if err := scalar(v); err != nil {
				return err
			}
			marks[i] = b.bind(v)
		}
		clause = col + " IN (" + strings.Join(marks, ", ") + ")"
	default:
		return driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("unknown operator %q", cond.Op))
	}
	b.where = append(b.where, clause)
	return nil
}

// scalar rejects values that are not a single comparable value.
func scalar(v driver.Value) error {
	switch v.Type() {
	case driver.TypeList, driver.TypeObject:
		return driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("a %s value is not valid here", v.Type()))
	}
	if _, err := bindValue(v); err != nil {
		return driver.Wrap(driver.ErrUnsupported, Name, "browse", err)
	}
	return nil
}

func parseOffsetCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	bad := driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor"))
	if len(cursor) > maxCursorLen {
		return 0, bad
	}
	digits, ok := strings.CutPrefix(cursor, offsetPrefix)
	n, err := strconv.Atoi(digits)
	if !ok || err != nil || n < 0 {
		return 0, bad
	}
	return n, nil
}

// pageResultSet adds the next-page cursor to the inner result's stats.
type pageResultSet struct {
	driver.ResultSet
	next int // offset of the next page
}

func (p *pageResultSet) Stats() driver.Stats {
	st := p.ResultSet.Stats()
	if st.Truncated {
		st.NextCursor = offsetPrefix + strconv.Itoa(p.next)
	}
	return st
}
