package drivertest

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/xavidop/tobira/driver"
)

// The Fake query language (used only by the Fake). Keywords are case
// insensitive; statements may be separated by ';' outside string literals.
//
//	select * from [ns.]coll [where f = lit] [order by f [asc|desc]] [limit N]
//	insert into c ... | update c ... | delete from c [where f = lit]    writes
//	sleep <time.ParseDuration text>
//	search [ns.]coll for 'term'     full-text hits: a _score column (Float), then the row
//
// A search hit is a row with the term as a whole word, any case, in one
// of its String values; _score counts the occurrences, and hits come best
// first (ties in stored order). It honours Query.Limit.
//
// A literal is an integer, a decimal, a 'single quoted' string ('' escapes a
// quote), true, false, null, or a placeholder: $N (1-based) or ? (next
// positional). "= null" matches null values. An unknown field is ErrNotFound,
// any other malformed text is ErrSyntax.

type tokKind uint8

const (
	tokWord tokKind = iota
	tokNum
	tokStr
	tokSym
	tokParam
)

type token struct {
	kind tokKind
	text string // for tokStr: the unescaped string
}

func (t token) isWord(w string) bool { return t.kind == tokWord && strings.EqualFold(t.text, w) }

// splitStatements splits text on ';' outside single-quoted strings, dropping
// empty statements.
func splitStatements(text string) []string {
	var out []string
	start, inStr := 0, false
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '\'':
			inStr = !inStr // '' toggles twice, which is correct
		case text[i] == ';' && !inStr:
			out = append(out, text[start:i])
			start = i + 1
		}
	}
	out = append(out, text[start:])
	out = slices.DeleteFunc(out, func(s string) bool { return strings.TrimSpace(s) == "" })
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func tokenize(op, s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '\'':
			var sb strings.Builder
			j := i + 1
			closed := false
			for j < len(s) {
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						sb.WriteByte('\'')
						j += 2
						continue
					}
					closed = true
					j++
					break
				}
				sb.WriteByte(s[j])
				j++
			}
			if !closed {
				return nil, fakeErr(driver.ErrSyntax, op, "unterminated string literal")
			}
			toks = append(toks, token{tokStr, sb.String()})
			i = j
		case unicode.IsDigit(c) || (c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
			j := i + 1
			for j < len(s) && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			toks = append(toks, token{tokNum, s[i:j]})
			i = j
		case unicode.IsLetter(c) || c == '_':
			j := i + 1
			for j < len(s) && (s[j] == '_' || s[j] == '.' || unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j]))) {
				j++
			}
			toks = append(toks, token{tokWord, s[i:j]})
			i = j
		case c == '$':
			j := i + 1
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j == i+1 {
				return nil, fakeErr(driver.ErrSyntax, op, "bad placeholder at offset %d", i)
			}
			toks = append(toks, token{tokParam, s[i:j]})
			i = j
		case c == '?':
			toks = append(toks, token{tokParam, "?"})
			i++
		case c == '=' || c == '*' || c == ',':
			toks = append(toks, token{tokSym, string(c)})
			i++
		default:
			return nil, fakeErr(driver.ErrSyntax, op, "unexpected character %q at offset %d", c, i)
		}
	}
	return toks, nil
}

// cond is "field = value".
type cond struct {
	field string
	val   driver.Value
}

func (c cond) matches(v driver.Value) bool {
	if c.val.IsNull() {
		return v.IsNull()
	}
	return EqualValue(c.val, v)
}

type stmtKind uint8

const (
	stmtSelect stmtKind = iota
	stmtWrite
	stmtSleep
	stmtSearch
)

type stmt struct {
	kind  stmtKind
	verb  string // first keyword, lower case
	ref   driver.CollectionRef
	where *cond
	order string
	desc  bool
	limit int // -1 = none
	dur   time.Duration
	del   bool   // a recognised "delete from c where f = lit": really deletes
	term  string // stmtSearch: the word looked for
}

// parser walks a token slice; params are shared across the statements of one query.
type parser struct {
	op      string
	toks    []token
	pos     int
	params  []driver.Value
	nextPos *int
}

func (p *parser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{}, false
}

func (p *parser) acceptWord(w string) bool {
	if t, ok := p.peek(); ok && t.isWord(w) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectWord(w string) error {
	if !p.acceptWord(w) {
		return fakeErr(driver.ErrSyntax, p.op, "expected %q", w)
	}
	return nil
}

func (p *parser) ident() (string, error) {
	t, ok := p.peek()
	if !ok || t.kind != tokWord {
		return "", fakeErr(driver.ErrSyntax, p.op, "expected a name")
	}
	p.pos++
	return t.text, nil
}

func (p *parser) literal() (driver.Value, error) {
	t, ok := p.peek()
	if !ok {
		return driver.Value{}, fakeErr(driver.ErrSyntax, p.op, "expected a value")
	}
	p.pos++
	switch t.kind {
	case tokNum:
		if strings.Contains(t.text, ".") {
			return driver.Decimal(t.text), nil
		}
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return driver.Decimal(t.text), nil
		}
		return driver.Int(n), nil
	case tokStr:
		return driver.String(t.text), nil
	case tokWord:
		switch strings.ToLower(t.text) {
		case "true":
			return driver.Bool(true), nil
		case "false":
			return driver.Bool(false), nil
		case "null":
			return driver.Null(), nil
		}
	case tokParam:
		i := 0
		if t.text == "?" {
			i = *p.nextPos
			*p.nextPos++
		} else {
			n, _ := strconv.Atoi(t.text[1:])
			i = n - 1
		}
		if i < 0 || i >= len(p.params) {
			return driver.Value{}, fakeErr(driver.ErrSyntax, p.op, "missing value for placeholder %s", t.text)
		}
		return p.params[i], nil
	}
	return driver.Value{}, fakeErr(driver.ErrSyntax, p.op, "expected a literal, got %q", t.text)
}

func (p *parser) where() (*cond, error) {
	field, err := p.ident()
	if err != nil {
		return nil, err
	}
	if t, ok := p.peek(); !ok || t.kind != tokSym || t.text != "=" {
		return nil, fakeErr(driver.ErrSyntax, p.op, "expected '='")
	}
	p.pos++
	v, err := p.literal()
	if err != nil {
		return nil, err
	}
	return &cond{field: field, val: v}, nil
}

func (p *parser) done() error {
	if p.pos < len(p.toks) {
		return fakeErr(driver.ErrSyntax, p.op, "unexpected %q", p.toks[p.pos].text)
	}
	return nil
}

func splitRef(name string) driver.CollectionRef {
	if ns, n, ok := strings.Cut(name, "."); ok {
		return driver.CollectionRef{Namespace: ns, Name: n}
	}
	return driver.CollectionRef{Name: name}
}

var writeRe = map[string]*regexp.Regexp{
	"insert": regexp.MustCompile(`(?is)^insert\s+into\s+([A-Za-z_][\w.]*)`),
	"update": regexp.MustCompile(`(?is)^update\s+([A-Za-z_][\w.]*)`),
	"delete": regexp.MustCompile(`(?is)^delete\s+from\s+([A-Za-z_][\w.]*)(.*)$`),
}

// verbOf returns the leading keyword of text (lower case) and the rest.
func verbOf(text string) (verb, rest string) {
	text = strings.TrimSpace(text)
	n := 0
	for n < len(text) && (text[n] >= 'a' && text[n] <= 'z' || text[n] >= 'A' && text[n] <= 'Z') {
		n++
	}
	return strings.ToLower(text[:n]), text[n:]
}

// parseStmt parses one statement. ns is the Query.Namespace.
func parseStmt(op, text string, params []driver.Value, nextPos *int, ns string) (*stmt, error) {
	text = strings.TrimSpace(text)
	verb, rest := verbOf(text)
	switch verb {
	case "sleep":
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return nil, fakeErr(driver.ErrSyntax, op, "bad sleep duration: %v", err)
		}
		return &stmt{kind: stmtSleep, verb: verb, dur: d}, nil
	case "insert", "update", "delete":
		m := writeRe[verb].FindStringSubmatch(text)
		if m == nil {
			return nil, fakeErr(driver.ErrSyntax, op, "malformed %s statement", verb)
		}
		st := &stmt{kind: stmtWrite, verb: verb, ref: splitRef(m[1])}
		if st.ref.Namespace == "" {
			st.ref.Namespace = ns
		}
		if verb == "delete" && strings.TrimSpace(m[2]) != "" {
			toks, err := tokenize(op, m[2])
			if err != nil {
				return nil, err
			}
			p := &parser{op: op, toks: toks, params: params, nextPos: nextPos}
			if err := p.expectWord("where"); err != nil {
				return nil, err
			}
			if st.where, err = p.where(); err != nil {
				return nil, err
			}
			if err := p.done(); err != nil {
				return nil, err
			}
			st.del = true
		}
		return st, nil
	case "search":
		return parseSearch(op, text, params, nextPos, ns)
	case "select":
	default:
		return nil, fakeErr(driver.ErrSyntax, op, "unrecognised statement %q", verb)
	}

	toks, err := tokenize(op, text)
	if err != nil {
		return nil, err
	}
	p := &parser{op: op, toks: toks, pos: 1, params: params, nextPos: nextPos}
	if t, ok := p.peek(); !ok || t.kind != tokSym || t.text != "*" {
		return nil, fakeErr(driver.ErrSyntax, op, "only 'select *' is supported")
	}
	p.pos++
	if err := p.expectWord("from"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	st := &stmt{kind: stmtSelect, verb: verb, ref: splitRef(name), limit: -1}
	if st.ref.Namespace == "" {
		st.ref.Namespace = ns
	}
	if p.acceptWord("where") {
		if st.where, err = p.where(); err != nil {
			return nil, err
		}
	}
	if p.acceptWord("order") {
		if err := p.expectWord("by"); err != nil {
			return nil, err
		}
		if st.order, err = p.ident(); err != nil {
			return nil, err
		}
		if p.acceptWord("desc") {
			st.desc = true
		} else {
			p.acceptWord("asc")
		}
	}
	if p.acceptWord("limit") {
		t, ok := p.peek()
		n, convErr := strconv.Atoi(t.text)
		if !ok || t.kind != tokNum || convErr != nil || n < 0 {
			return nil, fakeErr(driver.ErrSyntax, op, "limit needs a non-negative integer")
		}
		p.pos++
		st.limit = n
	}
	if err := p.done(); err != nil {
		return nil, err
	}
	return st, nil
}

// parseSearch parses "search [ns.]coll for 'term'".
func parseSearch(op, text string, params []driver.Value, nextPos *int, ns string) (*stmt, error) {
	toks, err := tokenize(op, text)
	if err != nil {
		return nil, err
	}
	p := &parser{op: op, toks: toks, pos: 1, params: params, nextPos: nextPos}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	if err := p.expectWord("for"); err != nil {
		return nil, err
	}
	v, err := p.literal()
	if err != nil {
		return nil, err
	}
	term, ok := v.AsText()
	if !ok || v.Type() != driver.TypeString || strings.TrimSpace(term) == "" {
		return nil, fakeErr(driver.ErrSyntax, op, "search needs a non-empty 'term'")
	}
	if err := p.done(); err != nil {
		return nil, err
	}
	st := &stmt{kind: stmtSearch, verb: "search", ref: splitRef(name), limit: -1, term: strings.ToLower(term)}
	if st.ref.Namespace == "" {
		st.ref.Namespace = ns
	}
	return st, nil
}

// searchRows returns the hits of term in t (rows holding it as a whole
// word, any case, in a String value) best first, with their scores.
func searchRows(t *table, term string) ([]driver.Row, []float64) {
	type hit struct {
		row   driver.Row
		score float64
	}
	var hits []hit
	for _, r := range t.rows {
		n := 0
		for _, v := range r {
			if s, ok := v.AsText(); ok && v.Type() == driver.TypeString {
				for _, w := range strings.FieldsFunc(strings.ToLower(s), func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) }) {
					if w == term {
						n++
					}
				}
			}
		}
		if n > 0 {
			hits = append(hits, hit{r, float64(n)})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return cmp.Compare(b.score, a.score) })
	rows, scores := make([]driver.Row, len(hits)), make([]float64, len(hits))
	for i, h := range hits {
		rows[i], scores[i] = h.row, h.score
	}
	return rows, scores
}

// query runs a (possibly multi-statement) query.
func (s *session) query(ctx context.Context, q driver.Query) (rs driver.ResultSet, err error) {
	idx, err := s.enter(ctx, "query", driver.CollectionRef{}, q)
	defer s.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	return s.runQuery(ctx, q)
}

// runQuery is query without the call bookkeeping (Export reuses it).
func (s *session) runQuery(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	var err error
	s.f.mu.Lock()
	var handler func(driver.Query) (driver.ResultSet, error)
	for _, h := range s.f.handlers {
		if (h.re != nil && h.re.MatchString(q.Text)) || (h.re == nil && h.exact == q.Text) {
			handler = h.fn
			break
		}
	}
	s.f.mu.Unlock()
	if handler != nil {
		return handler(q)
	}

	texts := splitStatements(q.Text)
	if len(texts) == 0 {
		return nil, fakeErr(driver.ErrSyntax, "query", "empty query")
	}
	next := 0
	stmts := make([]*stmt, len(texts))
	for i, t := range texts {
		if stmts[i], err = parseStmt("query", t, q.Params, &next, q.Namespace); err != nil {
			return nil, err
		}
	}
	m := &multiRS{}
	for _, st := range stmts {
		one, err := s.exec(ctx, st, q)
		if err != nil {
			_ = m.Close()
			return nil, err
		}
		m.sets = append(m.sets, one)
	}
	return m, nil
}

// exec runs one parsed statement.
func (s *session) exec(ctx context.Context, st *stmt, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	switch st.kind {
	case stmtSleep:
		timer := time.NewTimer(st.dur)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctxErr("query", ctx.Err())
		}
		// A column-less statement that writes nothing, like DDL in sqlite
		// and postgres: zero rows affected.
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: 0, Duration: time.Since(start)}), nil

	case stmtWrite:
		if s.ro || q.ReadOnly {
			return nil, fakeErr(driver.ErrReadOnly, "query", "%s refused: read-only", st.verb)
		}
		affected, err := s.write(st)
		if err != nil {
			return nil, err
		}
		return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: affected, Duration: time.Since(start)}), nil
	}

	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if why := s.closed(); why != "" {
		return nil, fakeErr(driver.ErrConnection, "query", "%s", why)
	}
	ref, t, err := s.tables().resolve("query", st.ref)
	if err != nil {
		return nil, err
	}
	if st.kind == stmtSearch {
		return searchResult(ref, t, st.term, q.Limit, start), nil
	}
	rows, err := selectRows("query", t, st.where, st.order, st.desc)
	if err != nil {
		return nil, err
	}
	limit := st.limit
	if q.Limit > 0 && (limit < 0 || q.Limit < limit) {
		limit = q.Limit
	}
	truncated := false
	if limit >= 0 && len(rows) > limit {
		rows, truncated = rows[:limit], true
	}
	cols, out := t.result(ref, rows)
	return driver.NewSliceResultSet(cols, out,
		driver.Stats{RowsAffected: -1, Duration: time.Since(start), Truncated: truncated}), nil
}

// searchResult is the result of a search statement: _score, then the
// columns of the hits (as a select over them would list them).
func searchResult(ref driver.CollectionRef, t *table, term string, limit int, start time.Time) driver.ResultSet {
	rows, scores := searchRows(t, term)
	truncated := false
	if limit > 0 && len(rows) > limit {
		rows, scores, truncated = rows[:limit], scores[:limit], true
	}
	cols, out := t.result(ref, rows)
	cols = slices.Insert(cols, 0, driver.Column{Name: "_score", Type: driver.TypeFloat, NativeType: "float"})
	for i := range out {
		out[i] = slices.Insert(out[i], 0, driver.Float(scores[i]))
	}
	return driver.NewSliceResultSet(cols, out, driver.Stats{RowsAffected: -1, Duration: time.Since(start), Truncated: truncated})
}

// write applies a write statement: the collection must exist; a recognised
// delete really deletes (and is logged in a Tx); anything else is a no-op that
// reports one affected row.
func (s *session) write(st *stmt) (int64, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if why := s.closed(); why != "" { // a Commit may have won the race since enter
		return 0, fakeErr(driver.ErrConnection, "query", "%s", why)
	}
	ref, t, err := s.tables().resolve("query", st.ref)
	if err != nil {
		return 0, err
	}
	if !st.del {
		return 1, nil
	}
	ci := t.colIndex(st.where.field)
	if ci < 0 {
		return 0, fakeErr(driver.ErrNotFound, "query", "no such field %q", st.where.field)
	}
	before := len(t.rows)
	t.rows = slices.DeleteFunc(t.rows, func(r driver.Row) bool { return st.where.matches(r[ci]) })
	s.record(txOp{kind: "deleteWhere", ref: ref, where: st.where})
	return int64(before - len(t.rows)), nil
}

// selectRows filters and sorts a table's rows (the returned slice is new; the
// rows themselves are shared and must be cloned before leaving the Fake).
func selectRows(op string, t *table, where *cond, order string, desc bool) ([]driver.Row, error) {
	rows := slices.Clone(t.rows)
	if where != nil {
		ci := t.colIndex(where.field)
		if ci < 0 {
			return nil, fakeErr(driver.ErrNotFound, op, "no such field %q", where.field)
		}
		rows = slices.DeleteFunc(rows, func(r driver.Row) bool { return !where.matches(r[ci]) })
	}
	if order != "" {
		return sortRows(op, t, rows, []driver.SortKey{{Field: order, Desc: desc}})
	}
	return rows, nil
}

// sortRows is a stable multi-key sort; NULL sorts as the greatest value.
func sortRows(op string, t *table, rows []driver.Row, keys []driver.SortKey) ([]driver.Row, error) {
	idx := make([]int, len(keys))
	for i, k := range keys {
		if idx[i] = t.colIndex(k.Field); idx[i] < 0 {
			return nil, fakeErr(driver.ErrNotFound, op, "no such field %q", k.Field)
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		for i, k := range keys {
			c := compareNullsHigh(rows[a][idx[i]], rows[b][idx[i]])
			if k.Desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
	return rows, nil
}

// multiRS is the ResultSet of a query: one or more statement results.
type multiRS struct {
	sets []driver.ResultSet
	cur  int
}

func (m *multiRS) Columns() []driver.Column { return m.sets[m.cur].Columns() }
func (m *multiRS) Next(ctx context.Context) (driver.Row, bool, error) {
	return m.sets[m.cur].Next(ctx)
}
func (m *multiRS) Stats() driver.Stats { return m.sets[m.cur].Stats() }

func (m *multiRS) NextResultSet(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, ctxErr("query", err)
	}
	if m.cur+1 >= len(m.sets) {
		return false, nil
	}
	m.cur++
	return true, nil
}

func (m *multiRS) Close() error {
	for _, rs := range m.sets {
		_ = rs.Close()
	}
	return nil
}

var _ driver.MultiResultSet = (*multiRS)(nil)
