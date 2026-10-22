package duckdb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// defaultBrowseLimit is the page size when BrowseOptions.Limit is not set.
const defaultBrowseLimit = 100

// likeEscaper escapes the LIKE wildcards and the escape character itself.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Browse pages through a table or view with an optional filter and sort.
// It builds one SELECT and runs it through Query, so the single-statement
// and read-only checks apply to the generated text, Filter.Raw included.
// The cursor is a row offset; rows are ordered by the sort keys, then the
// primary key (or rowid for a table without one) so paging is stable.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	offset := 0
	if opts.Cursor != "" {
		n, err := strconv.Atoi(opts.Cursor)
		if err != nil || n < 0 {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("invalid cursor %q", opts.Cursor))
		}
		offset = n
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	schema, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	q, err := browseQuery(schema, opts, offset, limit)
	if err != nil {
		return nil, err
	}
	rs, err := c.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	return &pageResultSet{ResultSet: rs, next: offset + limit}, nil
}

// browseQuery builds the page's SELECT. Field names resolve against schema
// (unknown is ErrNotFound) and values are bound; Filter.Raw is pasted in
// parentheses. It asks for one row more than the page, so Limit marks it
// truncated.
func browseQuery(schema *driver.CollectionSchema, opts driver.BrowseOptions, offset, limit int) (driver.Query, error) {
	const op = "browse"
	field := func(name string) (string, error) { return column(op, schema, name) }
	var sb strings.Builder
	sb.WriteString("SELECT * FROM " + quoteIdent(schema.Ref.Namespace) + "." + quoteIdent(schema.Ref.Name))
	var where []string
	var params []driver.Value
	for _, cond := range opts.Filter.Conditions {
		col, err := field(cond.Field)
		if err != nil {
			return driver.Query{}, err
		}
		clause, args, err := conditionSQL(col, cond)
		if err != nil {
			return driver.Query{}, err
		}
		where = append(where, clause)
		params = append(params, args...)
	}
	if raw := opts.Filter.Raw; strings.TrimSpace(raw) != "" {
		where = append(where, "("+raw+")")
	}
	if len(where) > 0 {
		sb.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	var order []string
	for _, k := range opts.Sort {
		col, err := field(k.Field)
		if err != nil {
			return driver.Query{}, err
		}
		if k.Desc {
			order = append(order, col+" DESC NULLS FIRST")
		} else {
			order = append(order, col+" ASC NULLS LAST")
		}
	}
	switch {
	case len(schema.PrimaryKey) > 0:
		for _, pk := range schema.PrimaryKey {
			order = append(order, quoteIdent(pk))
		}
	case schema.Type == driver.CollectionTable && !hasField(schema, "rowid"):
		order = append(order, "rowid")
	}
	if len(order) > 0 {
		sb.WriteString(" ORDER BY " + strings.Join(order, ", "))
	}
	sb.WriteString(" LIMIT ? OFFSET ?")
	params = append(params, driver.Int(int64(limit+1)), driver.Int(int64(offset)))
	return driver.Query{Text: sb.String(), Params: params, Limit: limit, ReadOnly: opts.ReadOnly}, nil
}

func hasField(schema *driver.CollectionSchema, name string) bool {
	for _, f := range schema.Fields {
		if strings.EqualFold(f.Name, name) {
			return true
		}
	}
	return false
}

// conditionSQL renders one condition against the quoted column col.
func conditionSQL(col string, cond driver.Condition) (string, []driver.Value, error) {
	const op = "browse"
	switch cond.Op {
	case driver.OpIsNull:
		return col + " IS NULL", nil, nil
	case driver.OpNotNull:
		return col + " IS NOT NULL", nil, nil
	case driver.OpIn:
		list, ok := cond.Value.AsList()
		if !ok {
			return "", nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("in needs a list value"))
		}
		if len(list) == 0 {
			return "false", nil, nil
		}
		for _, v := range list {
			if err := bindable(v); err != nil {
				return "", nil, err
			}
		}
		return col + " IN (?" + strings.Repeat(", ?", len(list)-1) + ")", list, nil
	}
	if cond.Value.IsNull() { // "= NULL" matches nothing; mean IS [NOT] NULL
		switch cond.Op {
		case driver.OpEq:
			return col + " IS NULL", nil, nil
		case driver.OpNe:
			return col + " IS NOT NULL", nil, nil
		}
	}
	val := cond.Value
	var clause string
	switch cond.Op {
	case driver.OpEq:
		clause = col + " = ?"
	case driver.OpNe:
		clause = col + " <> ?"
	case driver.OpLt:
		clause = col + " < ?"
	case driver.OpLe:
		clause = col + " <= ?"
	case driver.OpGt:
		clause = col + " > ?"
	case driver.OpGe:
		clause = col + " >= ?"
	case driver.OpContains:
		clause = "CAST(" + col + ` AS VARCHAR) LIKE '%' || ? || '%' ESCAPE '\'`
		val = driver.String(likeEscaper.Replace(val.String()))
	case driver.OpPrefix:
		clause = "CAST(" + col + ` AS VARCHAR) LIKE ? || '%' ESCAPE '\'`
		val = driver.String(likeEscaper.Replace(val.String()))
	default:
		return "", nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("unknown operator %q", cond.Op))
	}
	if err := bindable(val); err != nil {
		return "", nil, err
	}
	return clause, []driver.Value{val}, nil
}

// bindable rejects values that have no parameter form here.
func bindable(v driver.Value) error {
	switch v.Type() {
	case driver.TypeVector, driver.TypeObject:
		return driver.Wrap(driver.ErrUnsupported, Name, "browse", fmt.Errorf("%s values cannot be bound", v.Type()))
	case driver.TypeList:
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("a list is only valid with the in operator"))
	}
	return nil
}

// column resolves a field name to the quoted column of schema: the exact
// name wins, else a unique case-insensitive match. Unknown or ambiguous is
// ErrNotFound.
func column(op string, schema *driver.CollectionSchema, field string) (string, error) {
	var folded []string
	for _, f := range schema.Fields {
		if f.Name == field {
			return quoteIdent(f.Name), nil
		}
		if strings.EqualFold(f.Name, field) {
			folded = append(folded, f.Name)
		}
	}
	if len(folded) == 1 {
		return quoteIdent(folded[0]), nil
	}
	if len(folded) > 1 {
		return "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("field %q is ambiguous in %s", field, schema.Ref.Name))
	}
	return "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no field %q in %s", field, schema.Ref.Name))
}

// pageResultSet adds the next-page cursor to the inner result's stats.
type pageResultSet struct {
	driver.ResultSet
	next int
}

func (p *pageResultSet) Stats() driver.Stats {
	st := p.ResultSet.Stats()
	if st.Truncated {
		st.NextCursor = strconv.Itoa(p.next)
	}
	return st
}
