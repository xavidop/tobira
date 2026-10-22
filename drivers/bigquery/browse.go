package bigquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	bq "cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultBrowseLimit = 100
	maxBrowseLimit     = 10000
	listPrefix         = "l:" // a tabledata.list cursor
	offsetPrefix       = "o:" // a query cursor
	maxCursorLen       = 4096
)

var _ driver.Browser = (*conn)(nil)

// Browse pages through a table or view.
//
//   - A table with no filter and no sort is read with tabledata.list, which
//     BigQuery does not bill: the cursor is the API's page token plus the
//     rows already returned from that page. (Real BigQuery returns at most
//     the page size per page, so that count stays 0 there; the emulator
//     ignores the page size and returns every row on one page.) Rows come
//     in storage order.
//   - A view, or any filter or sort, runs one SELECT with LIMIT/OFFSET,
//     ordered by the caller's sort, then the declared primary key, then
//     every column that can be ordered. That query is billed, and it
//     always passes both layers of the read-only guard, writable
//     connection or not, Filter.Raw included.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowseLimit
	}
	limit = min(limit, maxBrowseLimit)
	if len(opts.Cursor) > maxCursorLen {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("invalid cursor"))
	}
	sch, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, withOp(err, op)
	}
	plain := len(opts.Filter.Conditions) == 0 && strings.TrimSpace(opts.Filter.Raw) == "" && len(opts.Sort) == 0
	if plain && sch.Type == driver.CollectionTable && !strings.HasPrefix(opts.Cursor, offsetPrefix) {
		return c.browseList(ctx, sch, limit, opts.Cursor)
	}
	return c.browseQuery(ctx, sch, limit, opts)
}

// listCursor is a tabledata.list position: a page token and the rows of
// that page already returned.
type listCursor struct {
	Token string `json:"t,omitempty"`
	Skip  int    `json:"s,omitempty"`
}

func parseListCursor(s string) (listCursor, error) {
	var lc listCursor
	if s == "" {
		return lc, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, listPrefix))
	if !strings.HasPrefix(s, listPrefix) || err != nil || json.Unmarshal(raw, &lc) != nil || lc.Skip < 0 {
		return listCursor{}, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New("invalid cursor"))
	}
	return lc, nil
}

func (lc listCursor) String() string {
	b, _ := json.Marshal(lc)
	return listPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func (c *conn) browseList(ctx context.Context, sch *driver.CollectionSchema, limit int, cursor string) (driver.ResultSet, error) {
	const op = "browse"
	cur, err := parseListCursor(cursor)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	ictx, icancel := context.WithCancel(context.WithoutCancel(ctx))
	defer icancel()
	stop := context.AfterFunc(ctx, icancel)
	defer stop()
	it := c.client.Dataset(sch.Ref.Namespace).Table(sch.Ref.Name).Read(ictx)
	it.PageInfo().Token = cur.Token
	it.PageInfo().MaxSize = limit
	var (
		rows    []driver.Row
		vals    []bq.Value
		skipped int
		fetched bool
		anyCut  bool
	)
	for len(rows) < limit {
		if fetched && it.PageInfo().Remaining() == 0 {
			break // never read into the next page: its token is the cursor
		}
		err := it.Next(&vals)
		fetched = true
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, c.fail(ctx, op, err)
		}
		if skipped < cur.Skip {
			skipped++
			continue
		}
		row := make(driver.Row, len(it.Schema))
		for i, fs := range it.Schema {
			var v bq.Value
			if i < len(vals) {
				v = vals[i]
			}
			var cut bool
			row[i], cut = toValueCut(fs, v)
			anyCut = anyCut || cut
		}
		rows = append(rows, row)
	}
	next := ""
	switch {
	case fetched && it.PageInfo().Remaining() > 0:
		next = listCursor{Token: cur.Token, Skip: cur.Skip + len(rows)}.String()
	case it.PageInfo().Token != "":
		next = listCursor{Token: it.PageInfo().Token}.String()
	}
	cols := make([]driver.Column, len(it.Schema))
	for i, fs := range it.Schema {
		cols[i] = c.column(sch, fs)
	}
	stats := driver.Stats{RowsAffected: -1, Duration: time.Since(start), NextCursor: next}
	if anyCut {
		stats.Warnings = []string{cutWarning}
	}
	return driver.NewSliceResultSet(cols, rows, stats), nil
}

// column describes a result column of a browse over sch.
func (c *conn) column(sch *driver.CollectionSchema, fs *bq.FieldSchema) driver.Column {
	ref := sch.Ref
	return driver.Column{Name: fs.Name, NativeType: nativeType(fs), Type: fieldType(fs), Source: &ref, Key: slices.Contains(sch.PrimaryKey, fs.Name)}
}

func (c *conn) browseQuery(ctx context.Context, sch *driver.CollectionSchema, limit int, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	offset := 0
	if opts.Cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(opts.Cursor, offsetPrefix))
		if !strings.HasPrefix(opts.Cursor, offsetPrefix) || err != nil || n < 0 {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("invalid cursor"))
		}
		offset = n
	}
	byName := map[string]driver.Field{}
	for _, f := range sch.Fields {
		byName[strings.ToLower(f.Name)] = f
	}
	field := func(name string) (driver.Field, error) {
		f, ok := byName[strings.ToLower(name)]
		if !ok {
			return driver.Field{}, driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no column "+strconv.Quote(name)))
		}
		return f, nil
	}
	var (
		where  []string
		params []driver.Value
	)
	for _, cond := range opts.Filter.Conditions {
		f, err := field(cond.Field)
		if err != nil {
			return nil, err
		}
		expr, ps, err := condition(f, cond)
		if err != nil {
			return nil, err
		}
		where = append(where, expr)
		params = append(params, ps...)
	}
	if raw := opts.Filter.Raw; strings.TrimSpace(raw) != "" {
		if err := checkRawFilter(raw); err != nil {
			return nil, err
		}
		where = append(where, "("+raw+"\n)") // the newline ends a trailing line comment
	}
	var order []string
	seen := map[string]bool{}
	for _, k := range opts.Sort {
		f, err := field(k.Field)
		if err != nil {
			return nil, err
		}
		if !orderable(f) {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("cannot sort by "+f.NativeType+" column "+strconv.Quote(f.Name)))
		}
		dir := " ASC NULLS LAST"
		if k.Desc {
			dir = " DESC NULLS FIRST"
		}
		order = append(order, quoteIdent(f.Name)+dir)
		seen[f.Name] = true
	}
	tiebreak := slices.Clone(sch.PrimaryKey)
	for _, f := range sch.Fields {
		tiebreak = append(tiebreak, f.Name)
	}
	for _, name := range tiebreak {
		if f, ok := byName[strings.ToLower(name)]; ok && !seen[f.Name] && orderable(f) {
			order = append(order, quoteIdent(f.Name))
			seen[f.Name] = true
		}
	}
	var b strings.Builder
	b.WriteString("SELECT * FROM ")
	b.WriteString(quoteIdent(c.dsn.project) + "." + quoteIdent(sch.Ref.Namespace) + "." + quoteIdent(sch.Ref.Name))
	if len(where) > 0 {
		b.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	if len(order) > 0 {
		b.WriteString(" ORDER BY " + strings.Join(order, ", "))
	}
	b.WriteString(" LIMIT " + strconv.Itoa(limit+1) + " OFFSET " + strconv.Itoa(offset))
	rs, err := c.run(ctx, op, driver.Query{Text: b.String(), Params: params, Namespace: sch.Ref.Namespace}, true)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	rows, st, err := driver.Collect(ctx, rs, 0)
	_ = rs.Close()
	if err != nil {
		return nil, err
	}
	stats := driver.Stats{RowsAffected: -1, Duration: st.Duration + time.Since(start), Warnings: st.Warnings}
	if len(rows) > limit {
		rows = rows[:limit]
		stats.NextCursor = offsetPrefix + strconv.Itoa(offset+limit)
	}
	cols := slices.Clone(rs.Columns())
	ref := sch.Ref
	for i := range cols {
		cols[i].Source = &ref
		cols[i].Key = slices.Contains(sch.PrimaryKey, cols[i].Name)
	}
	return driver.NewSliceResultSet(cols, rows, stats), nil
}

// orderable reports whether GoogleSQL can ORDER BY a column of f's type.
func orderable(f driver.Field) bool {
	switch f.Type {
	case driver.TypeList, driver.TypeObject, driver.TypeJSON, driver.TypeUnknown:
		return false
	}
	return f.NativeType != "GEOGRAPHY" && !strings.HasPrefix(f.NativeType, "RANGE")
}

// condition renders one structured filter condition with ? parameters.
func condition(f driver.Field, cond driver.Condition) (string, []driver.Value, error) {
	col := quoteIdent(f.Name)
	unsupported := func(why string) (string, []driver.Value, error) {
		return "", nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New(why))
	}
	if f.Type == driver.TypeList || f.Type == driver.TypeObject {
		if cond.Op == driver.OpIsNull || cond.Op == driver.OpNotNull {
			if f.Type == driver.TypeList {
				return unsupported("an ARRAY is never NULL in BigQuery; filter on ARRAY_LENGTH in a raw filter")
			}
		} else {
			return unsupported("cannot compare a " + f.NativeType + " column; use a raw filter")
		}
	}
	switch cond.Op {
	case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		if f.Type == driver.TypeJSON {
			return unsupported("cannot compare a JSON column; use a raw filter with JSON functions")
		}
		ops := map[driver.Op]string{driver.OpEq: "=", driver.OpNe: "!=", driver.OpLt: "<", driver.OpLe: "<=", driver.OpGt: ">", driver.OpGe: ">="}
		return col + " " + ops[cond.Op] + " ?", []driver.Value{cond.Value}, nil
	case driver.OpContains, driver.OpPrefix:
		s, ok := cond.Value.AsText()
		if !ok {
			s = cond.Value.String()
		}
		s = escapeLike(s)
		pattern := s + "%"
		if cond.Op == driver.OpContains {
			pattern = "%" + pattern
		}
		text := "CAST(" + col + " AS STRING)"
		switch f.Type {
		case driver.TypeJSON:
			text = "TO_JSON_STRING(" + col + ")"
		case driver.TypeBytes:
			text = "SAFE_CONVERT_BYTES_TO_STRING(" + col + ")"
		}
		return text + " LIKE ?", []driver.Value{driver.String(pattern)}, nil
	case driver.OpIn:
		vs, ok := cond.Value.AsList()
		if !ok {
			return unsupported("OpIn needs a list value")
		}
		if len(vs) == 0 {
			return "FALSE", nil, nil
		}
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(vs)), ", ")
		return col + " IN (" + marks + ")", vs, nil
	case driver.OpIsNull:
		return col + " IS NULL", nil, nil
	case driver.OpNotNull:
		return col + " IS NOT NULL", nil, nil
	}
	return unsupported("unknown filter operator " + strconv.Quote(string(cond.Op)))
}

// escapeLike escapes LIKE's wildcards and its escape character.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// quoteIdent quotes a name with backticks, escaping backslashes and
// backticks.
func quoteIdent(s string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(s) + "`"
}

// checkRawFilter keeps a raw filter a WHERE body: it must lex, keep its
// parentheses balanced and hold no ';'. The whole generated query then
// passes the read-only guard.
func checkRawFilter(raw string) error {
	r, err := lex(raw)
	if err != nil {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("raw filter: "+err.Error()))
	}
	depth := 0
	for _, t := range r.toks {
		if t.kind != tokPunct {
			continue
		}
		switch t.text {
		case "(":
			depth++
		case ")":
			depth--
		case ";":
			return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("raw filter: ';' is not allowed"))
		}
		if depth < 0 {
			break
		}
	}
	if depth != 0 {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("raw filter: unbalanced parentheses"))
	}
	return nil
}
