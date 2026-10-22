package sqlitecore

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// DefaultBrowseLimit is the page size when BrowseOptions.Limit is not set.
const DefaultBrowseLimit = 100

// likeEscaper escapes the LIKE wildcards and the escape character itself.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// BrowsePage is one Browse call's paging: the row offset decoded from the
// cursor and the page size.
type BrowsePage struct {
	Offset int
	Limit  int
}

// ParseBrowse decodes opts' cursor (a row offset) and page size. A bad
// cursor is ErrSyntax.
func ParseBrowse(name string, opts driver.BrowseOptions) (BrowsePage, error) {
	p := BrowsePage{Limit: opts.Limit}
	if opts.Cursor != "" {
		n, err := strconv.Atoi(opts.Cursor)
		if err != nil || n < 0 {
			return p, driver.Wrap(driver.ErrSyntax, name, "browse", fmt.Errorf("invalid cursor %q", opts.Cursor))
		}
		p.Offset = n
	}
	if p.Limit <= 0 {
		p.Limit = DefaultBrowseLimit
	}
	return p, nil
}

// BrowseQuery builds the one SELECT that reads page of schema's collection
// with opts' filter and sort. Field names resolve case-insensitively against
// schema (unknown → ErrNotFound) and values are bound; Filter.Raw is pasted
// in parentheses, so the caller must run the query through its
// single-statement and read-only checks. Rows are ordered by the sort keys,
// then by the primary key (or a rowid alias) so paging is stable. The query
// asks for one row more than the page, so Limit marks it truncated.
func BrowseQuery(name string, schema *driver.CollectionSchema, opts driver.BrowseOptions, page BrowsePage) (driver.Query, error) {
	const op = "browse"
	fields := make(map[string]string, len(schema.Fields)) // lower-cased -> declared name
	for _, f := range schema.Fields {
		fields[strings.ToLower(f.Name)] = f.Name
	}
	field := func(fname string) (string, error) {
		if f, ok := fields[strings.ToLower(fname)]; ok {
			return QuoteIdent(f), nil
		}
		return "", driver.Wrap(driver.ErrNotFound, name, op, fmt.Errorf("no field %q in %s", fname, schema.Ref.Name))
	}

	var sb strings.Builder
	sb.WriteString("SELECT * FROM " + QuoteIdent(schema.Ref.Namespace) + "." + QuoteIdent(schema.Ref.Name))
	var where []string
	var params []driver.Value
	for _, cond := range opts.Filter.Conditions {
		col, err := field(cond.Field)
		if err != nil {
			return driver.Query{}, err
		}
		clause, args, err := conditionSQL(name, col, cond)
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
	switch { // tiebreak so paging is stable
	case len(schema.PrimaryKey) > 0:
		for _, pk := range schema.PrimaryKey {
			order = append(order, QuoteIdent(pk))
		}
	case schema.Type == driver.CollectionTable:
		if rid := RowidTiebreak(schema); rid != "" {
			order = append(order, rid)
		}
	}
	if len(order) > 0 {
		sb.WriteString(" ORDER BY " + strings.Join(order, ", "))
	}
	sb.WriteString(" LIMIT ? OFFSET ?")
	params = append(params, driver.Int(int64(page.Limit+1)), driver.Int(int64(page.Offset)))
	return driver.Query{Text: sb.String(), Params: params, Limit: page.Limit, ReadOnly: opts.ReadOnly}, nil
}

// Wrap adds the next page's cursor to rs's stats when rs is truncated.
func (p BrowsePage) Wrap(rs driver.ResultSet) driver.ResultSet {
	return &pageResultSet{ResultSet: rs, next: p.Offset + p.Limit}
}

// RowidTiebreak returns the first of rowid, _rowid_ and oid that no real
// column shadows (a column of that name would take the identifier over), or
// "" when all three are taken. Paging then has no tiebreak for that table.
func RowidTiebreak(schema *driver.CollectionSchema) string {
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		shadowed := false
		for _, f := range schema.Fields {
			if strings.EqualFold(f.Name, alias) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			return alias
		}
	}
	return ""
}

// conditionSQL renders one condition against the quoted column col.
func conditionSQL(name, col string, cond driver.Condition) (string, []driver.Value, error) {
	const op = "browse"
	switch cond.Op {
	case driver.OpIsNull:
		return col + " IS NULL", nil, nil
	case driver.OpNotNull:
		return col + " IS NOT NULL", nil, nil
	case driver.OpIn:
		list, ok := cond.Value.AsList()
		if !ok {
			return "", nil, driver.Wrap(driver.ErrSyntax, name, op, errors.New("in needs a list value"))
		}
		if len(list) == 0 {
			return "0", nil, nil
		}
		for _, v := range list {
			if err := bindable(name, v); err != nil {
				return "", nil, err
			}
		}
		return col + " IN (?" + strings.Repeat(",?", len(list)-1) + ")", list, nil
	}
	if cond.Value.IsNull() { // "= NULL" matches nothing in SQL; mean IS [NOT] NULL
		switch cond.Op {
		case driver.OpEq:
			return col + " IS NULL", nil, nil
		case driver.OpNe:
			return col + " IS NOT NULL", nil, nil
		}
	}
	var clause string
	val := cond.Value
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
		clause = col + ` LIKE '%'||?||'%' ESCAPE '\'`
		val = driver.String(likeEscaper.Replace(val.String()))
	case driver.OpPrefix:
		clause = col + ` LIKE ?||'%' ESCAPE '\'`
		val = driver.String(likeEscaper.Replace(val.String()))
	default:
		return "", nil, driver.Wrap(driver.ErrUnsupported, name, op, fmt.Errorf("unknown operator %q", cond.Op))
	}
	if err := bindable(name, val); err != nil {
		return "", nil, err
	}
	return clause, []driver.Value{val}, nil
}

// bindable rejects values that have no SQLite parameter form.
func bindable(name string, v driver.Value) error {
	switch v.Type() {
	case driver.TypeVector:
		return driver.Wrap(driver.ErrUnsupported, name, "browse", errors.New("vector values cannot be bound"))
	case driver.TypeList:
		return driver.Wrap(driver.ErrSyntax, name, "browse", errors.New("a list is only valid with the in operator"))
	}
	return nil
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
