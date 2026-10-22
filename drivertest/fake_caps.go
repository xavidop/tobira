package drivertest

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xavidop/tobira/driver"
)

var (
	_ driver.Browser    = (*conn)(nil)
	_ driver.Mutator    = (*conn)(nil)
	_ driver.Transactor = (*conn)(nil)
	_ driver.Explainer  = (*conn)(nil)
	_ driver.Explainer  = (*tx)(nil)
	_ driver.Completer  = (*conn)(nil)
	_ driver.Exporter   = (*conn)(nil)
	_ driver.Mutator    = (*tx)(nil)
	_ driver.Tx         = (*tx)(nil)
	_ driver.Supporter  = (*tx)(nil)
)

const defaultPageSize = 100

// ---- filter evaluation ----

// matchCondition applies one structured Condition to a field value.
func matchCondition(c driver.Condition, v driver.Value) (bool, error) {
	eq := func() bool {
		if c.Value.IsNull() {
			return v.IsNull()
		}
		return EqualValue(c.Value, v)
	}
	switch c.Op {
	case driver.OpEq:
		return eq(), nil
	case driver.OpNe:
		return !eq(), nil
	case driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		if v.IsNull() || c.Value.IsNull() {
			return false, nil
		}
		cmp := compareValues(v, c.Value)
		switch c.Op {
		case driver.OpLt:
			return cmp < 0, nil
		case driver.OpLe:
			return cmp <= 0, nil
		case driver.OpGt:
			return cmp > 0, nil
		}
		return cmp >= 0, nil
	case driver.OpContains:
		return !v.IsNull() && strings.Contains(idText(v), idText(c.Value)), nil
	case driver.OpPrefix:
		return !v.IsNull() && strings.HasPrefix(idText(v), idText(c.Value)), nil
	case driver.OpIn:
		items, _ := c.Value.AsList()
		return slices.ContainsFunc(items, func(it driver.Value) bool {
			return EqualValue(it, v) || (it.IsNull() && v.IsNull())
		}), nil
	case driver.OpIsNull:
		return v.IsNull(), nil
	case driver.OpNotNull:
		return !v.IsNull(), nil
	}
	return false, fakeErr(driver.ErrUnsupported, "browse", "filter op %q is not supported", c.Op)
}

// filterRows applies f to rows. Filter.Raw uses the Fake query language's
// where body ("field = literal").
func filterRows(op string, t *table, f driver.Filter, rows []driver.Row) ([]driver.Row, error) {
	rows = slices.Clone(rows)
	for _, c := range f.Conditions {
		switch c.Op {
		case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe, driver.OpContains,
			driver.OpPrefix, driver.OpIn, driver.OpIsNull, driver.OpNotNull:
		default:
			return nil, fakeErr(driver.ErrUnsupported, op, "filter op %q is not supported", c.Op)
		}
		ci := t.colIndex(c.Field)
		if ci < 0 {
			return nil, fakeErr(driver.ErrNotFound, op, "no such field %q", c.Field)
		}
		rows = slices.DeleteFunc(rows, func(r driver.Row) bool {
			ok, _ := matchCondition(c, r[ci])
			return !ok
		})
	}
	if strings.TrimSpace(f.Raw) != "" {
		toks, err := tokenize(op, f.Raw)
		if err != nil {
			return nil, err
		}
		p := &parser{op: op, toks: toks, nextPos: new(int)}
		w, err := p.where()
		if err == nil {
			err = p.done()
		}
		if err != nil {
			return nil, err
		}
		ci := t.colIndex(w.field)
		if ci < 0 {
			return nil, fakeErr(driver.ErrNotFound, op, "no such field %q", w.field)
		}
		rows = slices.DeleteFunc(rows, func(r driver.Row) bool { return !w.matches(r[ci]) })
	}
	return rows, nil
}

// ---- Browser ----

func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	return c.browse(ctx, ref, opts)
}

// browse pages with an offset cursor (a decimal string). Stats.Truncated is
// true exactly when NextCursor is set.
func (s *session) browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (rs driver.ResultSet, err error) {
	if err := s.need("browse", driver.CapBrowser); err != nil {
		return nil, err
	}
	idx, err := s.enter(ctx, "browse", ref, driver.Query{})
	defer s.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	ref, t, err := s.tables().resolve("browse", ref)
	if err != nil {
		return nil, err
	}
	offset := 0
	if opts.Cursor != "" {
		if offset, err = strconv.Atoi(opts.Cursor); err != nil || offset < 0 {
			return nil, fakeErr(driver.ErrSyntax, "browse", "invalid cursor %q", opts.Cursor)
		}
	}
	rows, err := filterRows("browse", t, opts.Filter, t.rows)
	if err != nil {
		return nil, err
	}
	if len(opts.Sort) > 0 {
		if rows, err = sortRows("browse", t, rows, opts.Sort); err != nil {
			return nil, err
		}
	}
	size := opts.Limit
	if size <= 0 {
		size = defaultPageSize
	}
	offset = min(offset, len(rows))
	end := min(offset+size, len(rows))
	st := driver.Stats{RowsAffected: -1}
	if end < len(rows) {
		st.NextCursor, st.Truncated = strconv.Itoa(end), true
	}
	st.Duration = time.Since(start)
	cols, page := t.result(ref, rows[offset:end])
	return driver.NewSliceResultSet(cols, page, st), nil
}

// ---- Mutator ----

func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	return c.insert(ctx, ref, values)
}
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	return c.update(ctx, ref, key, changes)
}
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	return c.delete(ctx, ref, key)
}

// mutate runs the common prologue of a Mutator call and, with f.mu held,
// calls fn on the resolved table. It records the call and refuses writes on a
// read-only session.
func (s *session) mutate(ctx context.Context, op string, ref driver.CollectionRef,
	fn func(ref driver.CollectionRef, t *table) error) (err error) {
	if err := s.need(op, driver.CapMutator); err != nil {
		return err
	}
	idx, err := s.enter(ctx, op, ref, driver.Query{})
	defer s.f.finish(idx, &err)
	if err != nil {
		return err
	}
	if s.ro {
		return fakeErr(driver.ErrReadOnly, op, "%s refused: read-only", op)
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if why := s.closed(); why != "" { // a Commit may have won the race since enter
		return fakeErr(driver.ErrConnection, op, "%s", why)
	}
	ref, t, err := s.tables().resolve(op, ref)
	if err != nil {
		return err
	}
	return fn(ref, t)
}

func keyObject(t *table, row driver.Row) driver.Object {
	names := t.key
	if len(names) == 0 {
		for _, c := range t.cols {
			names = append(names, c.Name)
		}
	}
	out := make(driver.Object, 0, len(names))
	for _, n := range names {
		out = append(out, driver.Member{Key: n, Value: row[t.colIndex(n)]})
	}
	return out
}

func rowObject(t *table, row driver.Row) driver.Object {
	out := make(driver.Object, len(t.cols))
	for i, c := range t.cols {
		out[i] = driver.Member{Key: c.Name, Value: row[i]}
	}
	return out
}

// matchKey returns the indexes of the rows matching every member of key.
func matchKey(op string, t *table, key driver.Object) ([]int, error) {
	if len(key) == 0 {
		return nil, fakeErr(driver.ErrSyntax, op, "empty key")
	}
	cis := make([]int, len(key))
	for i, m := range key {
		if cis[i] = t.colIndex(m.Key); cis[i] < 0 {
			return nil, fakeErr(driver.ErrNotFound, op, "no such field %q", m.Key)
		}
	}
	var out []int
	for ri, r := range t.rows {
		ok := true
		for i, m := range key {
			if !(cond{m.Key, m.Value}).matches(r[cis[i]]) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, ri)
		}
	}
	return out, nil
}

// uniqueKeyOne finds the single row addressed by key: none is ErrNotFound,
// several ErrConflict.
func uniqueKeyOne(op string, t *table, key driver.Object) (int, error) {
	hits, err := matchKey(op, t, key)
	switch {
	case err != nil:
		return 0, err
	case len(hits) == 0:
		return 0, fakeErr(driver.ErrNotFound, op, "no row matches the key")
	case len(hits) > 1:
		return 0, fakeErr(driver.ErrConflict, op, "key matches %d rows", len(hits))
	}
	return hits[0], nil
}

// keyTaken reports whether another row (not skip) has the same primary key as row.
func keyTaken(t *table, row driver.Row, skip int) bool {
	if len(t.key) == 0 {
		return false
	}
	for ri, r := range t.rows {
		if ri == skip {
			continue
		}
		same := true
		for _, k := range t.key {
			ci := t.colIndex(k)
			if !(cond{k, row[ci]}).matches(r[ci]) {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// doInsert adds a row to t. A missing single integer primary key is assigned
// max+1. It returns the key and the stored row.
func doInsert(t *table, values driver.Object) (driver.Object, driver.Row, error) {
	row := make(driver.Row, len(t.cols))
	for _, m := range values {
		ci := t.colIndex(m.Key)
		if ci < 0 {
			return nil, nil, fakeErr(driver.ErrNotFound, "insert", "no such field %q", m.Key)
		}
		row[ci] = m.Value
	}
	if len(t.key) == 1 {
		ci := t.colIndex(t.key[0])
		if row[ci].IsNull() && t.cols[ci].Type == driver.TypeInt {
			var maxID int64
			for _, r := range t.rows {
				if n, ok := r[ci].AsInt(); ok {
					maxID = max(maxID, n)
				}
			}
			row[ci] = driver.Int(maxID + 1)
		}
	}
	for _, k := range t.key {
		if row[t.colIndex(k)].IsNull() {
			return nil, nil, fakeErr(driver.ErrConstraint, "insert", "primary key %q is null", k)
		}
	}
	if keyTaken(t, row, -1) {
		return nil, nil, fakeErr(driver.ErrConstraint, "insert", "duplicate primary key")
	}
	t.rows = append(t.rows, row)
	return keyObject(t, row), row, nil
}

// doUpdate applies changes to the single row addressed by key and returns the new row.
func doUpdate(t *table, key, changes driver.Object) (driver.Row, error) {
	ri, err := uniqueKeyOne("update", t, key)
	if err != nil {
		return nil, err
	}
	next := slices.Clone(t.rows[ri])
	for _, m := range changes {
		ci := t.colIndex(m.Key)
		if ci < 0 {
			return nil, fakeErr(driver.ErrNotFound, "update", "no such field %q", m.Key)
		}
		next[ci] = m.Value
	}
	if keyTaken(t, next, ri) {
		return nil, fakeErr(driver.ErrConstraint, "update", "duplicate primary key")
	}
	t.rows[ri] = next
	return next, nil
}

// doDelete removes the single row addressed by key and returns it.
func doDelete(t *table, key driver.Object) (driver.Row, error) {
	ri, err := uniqueKeyOne("delete", t, key)
	if err != nil {
		return nil, err
	}
	gone := t.rows[ri]
	t.rows = slices.Delete(t.rows, ri, ri+1)
	return gone, nil
}

func (s *session) insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	var key driver.Object
	err := s.mutate(ctx, "insert", ref, func(ref driver.CollectionRef, t *table) error {
		k, row, err := doInsert(t, values)
		if err != nil {
			return err
		}
		key = k
		s.record(txOp{kind: "insert", ref: ref, values: rowObject(t, row)})
		s.emit(ref, "insert", k, driver.ObjectValue(rowObject(t, row)...))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

func (s *session) update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	return s.mutate(ctx, "update", ref, func(ref driver.CollectionRef, t *table) error {
		next, err := doUpdate(t, key, changes)
		if err != nil {
			return err
		}
		s.record(txOp{kind: "update", ref: ref, key: key, values: changes})
		s.emit(ref, "update", keyObject(t, next), driver.ObjectValue(changes...))
		return nil
	})
}

func (s *session) delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	return s.mutate(ctx, "delete", ref, func(ref driver.CollectionRef, t *table) error {
		gone, err := doDelete(t, key)
		if err != nil {
			return err
		}
		s.record(txOp{kind: "delete", ref: ref, key: key})
		s.emit(ref, "delete", keyObject(t, gone), driver.Null())
		return nil
	})
}

// ---- Transactor ----

// txOp is one logged mutation of a Tx, replayed against the live data at Commit.
type txOp struct {
	kind   string // insert | update | delete | deleteWhere
	ref    driver.CollectionRef
	key    driver.Object // update/delete: the key as given
	values driver.Object // insert: the full row; update: the changes
	where  *cond         // deleteWhere
}

// record logs op when the session is a Tx. f.mu must be held.
func (s *session) record(op txOp) {
	if s.log != nil {
		*s.log = append(*s.log, op)
	}
}

// tx is a copy-on-write transaction. Reads see a snapshot taken at Begin plus
// the Tx's own changes. Its mutations are logged and, at Commit, replayed
// against the live data under the Fake's lock, so writes other Conns made
// since Begin survive. If a logged update or delete no longer finds its row
// (or an insert collides), Commit fails with ErrConflict and applies nothing.
// Committing an empty or read-only Tx changes nothing. Watch events for the
// applied changes are emitted at Commit.
type tx struct {
	session
	data     *tables
	ops      []txOp
	finished atomic.Bool
}

func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (_ driver.Tx, err error) {
	if err := c.need("begin", driver.CapTransactor); err != nil {
		return nil, err
	}
	idx, err := c.enter(ctx, "begin", driver.CollectionRef{}, driver.Query{})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	t := &tx{}
	c.f.mu.Lock()
	t.data = c.f.data.clone()
	c.f.mu.Unlock()
	t.session = session{
		f: c.f, ro: c.ro || opts.ReadOnly, data: &t.data, log: &t.ops, done: c.done,
		closed: func() string {
			if t.finished.Load() {
				return "transaction is finished"
			}
			return c.closed()
		},
	}
	return t, nil
}

// Supports implements driver.Supporter.
func (t *tx) Supports(cp driver.Capability) bool { return !t.f.disabled[cp] }

func (t *tx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return t.query(ctx, q)
}
func (t *tx) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	return t.insert(ctx, ref, values)
}
func (t *tx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	return t.update(ctx, ref, key, changes)
}
func (t *tx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	return t.delete(ctx, ref, key)
}

func (t *tx) Commit(ctx context.Context) (err error) {
	idx, err := t.enter(ctx, "commit", driver.CollectionRef{}, driver.Query{})
	defer t.f.finish(idx, &err)
	if err != nil {
		return err
	}
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	if t.finished.Load() {
		return fakeErr(driver.ErrConnection, "commit", "transaction is finished")
	}
	if len(t.ops) > 0 {
		work := t.f.data.clone()
		var events []pendingEvt
		for _, op := range t.ops {
			evs, err := applyOp(work, op)
			if err != nil {
				return fakeErr(driver.ErrConflict, "commit", "%v", err)
			}
			events = append(events, evs...)
		}
		t.f.data = work
		for _, p := range events {
			t.f.emitLocked(p.ref, p.ev)
		}
	}
	t.ops = nil
	t.finished.Store(true)
	return nil
}

// applyOp replays one logged mutation on d and returns the events it produced.
func applyOp(d *tables, op txOp) ([]pendingEvt, error) {
	ref, t, err := d.resolve(op.kind, op.ref)
	if err != nil {
		return nil, err
	}
	ev := func(o string, key driver.Object, payload driver.Value) pendingEvt {
		return pendingEvt{ref, driver.Event{Time: time.Now(), Op: o, Channel: ref.Name, Key: key, Payload: payload}}
	}
	switch op.kind {
	case "insert":
		key, row, err := doInsert(t, op.values)
		if err != nil {
			return nil, err
		}
		return []pendingEvt{ev("insert", key, driver.ObjectValue(rowObject(t, row)...))}, nil
	case "update":
		next, err := doUpdate(t, op.key, op.values)
		if err != nil {
			return nil, err
		}
		return []pendingEvt{ev("update", keyObject(t, next), driver.ObjectValue(op.values...))}, nil
	case "delete":
		gone, err := doDelete(t, op.key)
		if err != nil {
			return nil, err
		}
		return []pendingEvt{ev("delete", keyObject(t, gone), driver.Null())}, nil
	}
	// deleteWhere
	ci := t.colIndex(op.where.field)
	if ci < 0 {
		return nil, fakeErr(driver.ErrNotFound, "commit", "no such field %q", op.where.field)
	}
	var evs []pendingEvt
	t.rows = slices.DeleteFunc(t.rows, func(r driver.Row) bool {
		if !op.where.matches(r[ci]) {
			return false
		}
		evs = append(evs, ev("delete", keyObject(t, r), driver.Null()))
		return true
	})
	return evs, nil
}

// Rollback discards the transaction. After Commit or Rollback it is a no-op;
// that path only records the call, bypassing latency and FailNext, because a
// finished Tx would otherwise report ErrConnection.
func (t *tx) Rollback(ctx context.Context) (err error) {
	if t.finished.Load() {
		t.f.mu.Lock()
		t.f.calls = append(t.f.calls, Call{Op: "rollback"})
		t.f.mu.Unlock()
		return nil
	}
	idx, err := t.enter(ctx, "rollback", driver.CollectionRef{}, driver.Query{})
	defer t.f.finish(idx, &err)
	if err != nil {
		return err
	}
	t.f.mu.Lock()
	t.ops = nil
	t.finished.Store(true)
	t.f.mu.Unlock()
	return nil
}

// ---- Explainer ----

func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	return c.explain(ctx, q, opts)
}

// Explain plans q inside the transaction, against its snapshot and its own
// changes, like the Conn's Explain.
func (t *tx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	return t.explain(ctx, q, opts)
}

func (c *session) explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (_ *driver.Plan, err error) {
	if err := c.need("explain", driver.CapExplainer); err != nil {
		return nil, err
	}
	idx, err := c.enter(ctx, "explain", driver.CollectionRef{}, q)
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	texts := splitStatements(q.Text)
	if len(texts) == 0 {
		return nil, fakeErr(driver.ErrSyntax, "explain", "empty query")
	}
	next := 0
	st, err := parseStmt("explain", texts[0], q.Params, &next, q.Namespace)
	if err != nil {
		return nil, err
	}
	switch st.kind {
	case stmtWrite:
		if opts.Analyze && (c.ro || q.ReadOnly) {
			return nil, fakeErr(driver.ErrReadOnly, "explain", "analyze of a write refused: read-only")
		}
		return plan("FAKE WRITE "+strings.ToUpper(st.verb), "Write", st.verb, -1), nil
	case stmtSleep:
		return plan("FAKE SLEEP "+st.dur.String(), "Sleep", st.dur.String(), -1), nil
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	_, t, err := c.tables().resolve("explain", st.ref)
	if err != nil {
		return nil, err
	}
	return plan("FAKE SCAN "+st.ref.Name, "Scan", st.ref.Name, int64(len(t.rows))), nil
}

func plan(text, label, detail string, rows int64) *driver.Plan {
	return &driver.Plan{Format: "text", Text: text, Root: &driver.PlanNode{Label: label, Detail: detail, Rows: rows}}
}

// ---- Completer ----

var fakeKeywords = []string{"select", "from", "where", "order by", "limit", "asc", "desc",
	"insert", "update", "delete", "sleep", "true", "false", "null"}

var fromRe = regexp.MustCompile(`(?i)\bfrom\s+(\S+)`)

// Complete suggests keywords, namespaces, collections and fields whose name
// starts with the word before the cursor. Fields are limited to the collection
// named after "from" when the text has one.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) (_ []driver.Completion, err error) {
	if err := c.need("complete", driver.CapCompleter); err != nil {
		return nil, err
	}
	idx, err := c.enter(ctx, "complete", driver.CollectionRef{Namespace: req.Namespace}, driver.Query{Text: req.Text})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	cur := max(0, min(req.Cursor, len(req.Text)))
	start := cur
	for start > 0 && isWordByte(req.Text[start-1]) {
		start--
	}
	prefix := strings.ToLower(req.Text[start:cur])
	var out []driver.Completion
	add := func(label, kind, detail string) {
		if strings.HasPrefix(strings.ToLower(label), prefix) {
			out = append(out, driver.Completion{Label: label, Kind: kind, Detail: detail})
		}
	}
	for _, k := range fakeKeywords {
		add(k, "keyword", "")
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	d := c.tables()
	for _, ns := range d.namespaces() {
		add(ns, "namespace", "")
	}
	from := ""
	if m := fromRe.FindStringSubmatch(req.Text[:start]); m != nil {
		from = m[1]
	}
	seen := map[string]bool{}
	for _, ref := range d.order {
		if req.Namespace != "" && ref.Namespace != req.Namespace {
			continue
		}
		add(ref.Name, "collection", ref.Namespace)
		if from != "" && from != ref.Name && from != ref.Namespace+"."+ref.Name {
			continue
		}
		for _, col := range d.m[ref].cols {
			if !seen[col.Name] {
				seen[col.Name] = true
				add(col.Name, "field", ref.Name)
			}
		}
	}
	return out, nil
}

func isWordByte(b byte) bool {
	return b == '_' || b == '.' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ---- Exporter ----

// ExportFormats lists the native export formats.
func (c *conn) ExportFormats() []string { return []string{"csv", "ndjson"} }

// Export runs q and writes the first result set as csv (NULL is written as an
// empty field, so csv cannot tell NULL from "") or ndjson (one object per row, NULL as null).
func (c *conn) Export(ctx context.Context, q driver.Query, format string, w io.Writer) (err error) {
	if err := c.need("export", driver.CapExporter); err != nil {
		return err
	}
	idx, err := c.enter(ctx, "export", driver.CollectionRef{}, q)
	defer c.f.finish(idx, &err)
	if err != nil {
		return err
	}
	if !slices.Contains(c.ExportFormats(), format) {
		return fakeErr(driver.ErrUnsupported, "export", "unknown format %q", format)
	}
	rs, err := c.runQuery(ctx, q)
	if err != nil {
		return err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return ctxOr(err)
	}
	cols := rs.Columns()
	if format == "csv" {
		cw := csv.NewWriter(w)
		head := make([]string, len(cols))
		for i, col := range cols {
			head[i] = col.Name
		}
		_ = cw.Write(head)
		for _, r := range rows {
			rec := make([]string, len(r))
			for i, v := range r {
				if !v.IsNull() {
					rec[i] = idText(v)
				}
			}
			_ = cw.Write(rec)
		}
		cw.Flush()
		return wrapWrite(cw.Error())
	}
	for _, r := range rows {
		var sb strings.Builder
		sb.WriteByte('{')
		for i, v := range r {
			if i > 0 {
				sb.WriteByte(',')
			}
			k, _ := json.Marshal(cols[i].Name)
			vj, _ := v.MarshalJSON()
			sb.Write(k)
			sb.WriteByte(':')
			sb.Write(vj)
		}
		sb.WriteString("}\n")
		if _, err := io.WriteString(w, sb.String()); err != nil {
			return wrapWrite(err)
		}
	}
	return nil
}

func ctxOr(err error) error {
	var de *driver.Error
	if errors.As(err, &de) {
		return err
	}
	return driver.Wrap(nil, fakeName, "export", err)
}

func wrapWrite(err error) error {
	if err == nil {
		return nil
	}
	return driver.Wrap(nil, fakeName, "export", err)
}
