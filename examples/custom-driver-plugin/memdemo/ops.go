package memdemo

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// The operations below run on a data value. A Conn runs them on the store
// (writes on a copy the store keeps only on success); a Tx runs them on
// its snapshot. They check everything before they change anything.

// defaultPageSize is the Browse page size when BrowseOptions.Limit is 0.
const defaultPageSize = 100

func browse(d data, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	start := time.Now()
	t, err := d.table("browse", ref)
	if err != nil {
		return nil, err
	}
	if opts.Filter.Raw != "" {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "browse", errors.New("raw filters: use structured conditions"))
	}
	var rows []driver.Row
	for _, r := range t.rows {
		ok, err := matchesAll(t, r, opts.Filter.Conditions)
		if err != nil {
			return nil, err
		}
		if ok {
			rows = append(rows, r)
		}
	}
	if err := sortRows(t, rows, opts.Sort); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	offset := 0
	if opts.Cursor != "" {
		// The cursor is opaque to callers; here it is just the offset.
		if offset, err = strconv.Atoi(opts.Cursor); err != nil || offset < 0 {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "browse", fmt.Errorf("bad cursor %q", opts.Cursor))
		}
	}
	offset = min(offset, len(rows))
	end := min(offset+limit, len(rows))
	next := ""
	if end < len(rows) {
		next = strconv.Itoa(end)
	}
	return driver.NewSliceResultSet(resultColumns(t, driver.CollectionRef{Namespace: Namespace, Name: ref.Name}),
		rows[offset:end], driver.Stats{
			RowsAffected: -1, Duration: time.Since(start), Truncated: next != "", NextCursor: next,
		}), nil
}

// matchesAll reports whether row r satisfies every condition (they are
// ANDed).
func matchesAll(t *table, r driver.Row, conds []driver.Condition) (bool, error) {
	for _, c := range conds {
		i, ok := t.col(c.Field)
		if !ok {
			return false, driver.Wrap(driver.ErrNotFound, Name, "browse", fmt.Errorf("field %q", c.Field))
		}
		ok, err := matches(r[i], c)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// sortRows orders rows by the sort keys. NULLs sort last in either
// direction; values compare cannot order fall back to their text.
func sortRows(t *table, rows []driver.Row, keys []driver.SortKey) error {
	idx := make([]int, len(keys))
	for k, key := range keys {
		i, ok := t.col(key.Field)
		if !ok {
			return driver.Wrap(driver.ErrNotFound, Name, "browse", fmt.Errorf("sort field %q", key.Field))
		}
		idx[k] = i
	}
	slices.SortStableFunc(rows, func(a, b driver.Row) int {
		for k, i := range idx {
			x, y := a[i], b[i]
			switch {
			case x.IsNull() && y.IsNull():
				continue
			case x.IsNull():
				return 1
			case y.IsNull():
				return -1
			}
			n, ok := compare(x, y)
			if !ok {
				n = strings.Compare(x.String(), y.String())
			}
			if keys[k].Desc {
				n = -n
			}
			if n != 0 {
				return n
			}
		}
		return 0
	})
	return nil
}

// checkValue checks a value against its column: the column's type, or
// NULL for a column that is not the key.
func checkValue(op string, col driver.Column, v driver.Value) error {
	if v.IsNull() {
		if col.Key {
			return driver.Wrap(driver.ErrConstraint, Name, op, fmt.Errorf("key %q cannot be NULL", col.Name))
		}
		return nil
	}
	if v.Type() != col.Type {
		return driver.Wrap(driver.ErrConstraint, Name, op, fmt.Errorf("%q is %s, got %s", col.Name, col.Type, v.Type()))
	}
	return nil
}

// set puts the members of o into row, checking each.
func set(op string, t *table, row driver.Row, o driver.Object) error {
	for _, m := range o {
		i, ok := t.col(m.Key)
		if !ok {
			return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("field %q", m.Key))
		}
		if err := checkValue(op, t.cols[i], m.Value); err != nil {
			return err
		}
		row[i] = m.Value
	}
	return nil
}

func insert(d data, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	t, err := d.table("insert", ref)
	if err != nil {
		return nil, err
	}
	row := make(driver.Row, len(t.cols)) // every value starts NULL
	if err := set("insert", t, row, values); err != nil {
		return nil, err
	}
	k := t.keyIndex()
	if row[k].IsNull() {
		return nil, driver.Wrap(driver.ErrConstraint, Name, "insert", fmt.Errorf("key %q is required", t.key))
	}
	if _, dup := t.find(row[k]); dup {
		return nil, driver.Wrap(driver.ErrConstraint, Name, "insert", fmt.Errorf("duplicate key %s = %s", t.key, row[k]))
	}
	t.rows = append(t.rows, row)
	return driver.Object{{Key: t.key, Value: row[k]}}, nil
}

// locate finds the row a Mutator key names. The key is the primary key
// field and nothing else.
func locate(op string, d data, ref driver.CollectionRef, key driver.Object) (*table, int, error) {
	t, err := d.table(op, ref)
	if err != nil {
		return nil, 0, err
	}
	if len(key) != 1 || key[0].Key != t.key {
		return nil, 0, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("the key is the field %q", t.key))
	}
	i, ok := t.find(key[0].Value)
	if !ok {
		return nil, 0, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no row with %s = %s", t.key, key[0].Value))
	}
	return t, i, nil
}

func update(d data, ref driver.CollectionRef, key, changes driver.Object) error {
	t, i, err := locate("update", d, ref, key)
	if err != nil {
		return err
	}
	row := slices.Clone(t.rows[i]) // rows are replaced, never changed in place
	if err := set("update", t, row, changes); err != nil {
		return err
	}
	k := t.keyIndex()
	if j, dup := t.find(row[k]); dup && j != i {
		return driver.Wrap(driver.ErrConstraint, Name, "update", fmt.Errorf("duplicate key %s = %s", t.key, row[k]))
	}
	t.rows[i] = row
	return nil
}

func remove(d data, ref driver.CollectionRef, key driver.Object) error {
	t, i, err := locate("delete", d, ref, key)
	if err != nil {
		return err
	}
	t.rows = slices.Delete(t.rows, i, i+1)
	return nil
}

// explain describes how a statement would run: a scan, then a filter, then
// the write, as a tree and as indented text. With Analyze it runs a select
// (on d, which it does not change) and adds the actual row count; it never
// runs a write.
func explain(d data, q driver.Query, opts driver.ExplainOptions, readOnly bool) (*driver.Plan, error) {
	st, err := prepare(driver.Query{Text: q.Text, Params: q.Params, Namespace: q.Namespace}, false)
	if err != nil {
		return nil, err
	}
	if st.verb == "sleep" {
		if opts.Analyze {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "explain", errors.New("analyze of sleep"))
		}
		root := &driver.PlanNode{Label: "sleep", Detail: st.sleep.String(), Rows: 0}
		return &driver.Plan{Format: "text", Text: "sleep " + st.sleep.String() + "\n", Root: root}, nil
	}
	t, err := d.table("explain", driver.CollectionRef{Namespace: Namespace, Name: st.coll})
	if err != nil {
		return nil, err
	}
	if opts.Analyze && st.writes() {
		if readOnly || q.ReadOnly {
			return nil, errReadOnly("explain")
		}
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "explain", errors.New("analyze runs only select statements"))
	}

	root := &driver.PlanNode{Label: "scan", Detail: st.coll, Rows: int64(len(t.rows))}
	if st.verb == "reset" {
		root = &driver.PlanNode{Label: "reset", Detail: st.coll + " from its seed data", Rows: -1}
	}
	if st.where != nil {
		if _, ok := t.col(st.where.field); !ok {
			return nil, driver.Wrap(driver.ErrNotFound, Name, "explain", fmt.Errorf("field %q", st.where.field))
		}
		root = &driver.PlanNode{Label: "filter", Detail: st.where.field + " = " + st.where.value.String(), Rows: -1, Children: []*driver.PlanNode{root}}
	}
	if st.verb == "delete" {
		root = &driver.PlanNode{Label: "delete", Detail: st.coll, Rows: -1, Children: []*driver.PlanNode{root}}
	}
	if opts.Analyze {
		res, err := exec(d, st) // a select: it reads d and changes nothing
		if err != nil {
			return nil, err
		}
		root.Rows = int64(len(res.rows))
		root.Detail += fmt.Sprintf(" (actual rows %d)", len(res.rows))
	}
	var b strings.Builder
	writePlan(&b, root, 0)
	return &driver.Plan{Format: "text", Text: b.String(), Root: root}, nil
}

func writePlan(b *strings.Builder, n *driver.PlanNode, depth int) {
	fmt.Fprintf(b, "%s%s %s", strings.Repeat("  ", depth), n.Label, n.Detail)
	if n.Rows >= 0 {
		fmt.Fprintf(b, " rows=%d", n.Rows)
	}
	b.WriteByte('\n')
	for _, c := range n.Children {
		writePlan(b, c, depth+1)
	}
}

// keywords are the words of the language, for completion.
var keywords = []string{"select", "from", "where", "delete", "reset", "sleep", "true", "false", "null"}

// complete suggests the word under the cursor: a collection after from or
// reset, a field after where, else any keyword, collection or field.
func complete(d data, req driver.CompletionRequest) []driver.Completion {
	cursor := min(max(req.Cursor, 0), len(req.Text))
	before := req.Text[:cursor]
	start := strings.LastIndexAny(before, " \t\n\r*=;'") + 1
	prefix := strings.ToLower(before[start:])
	prev := ""
	if words := strings.Fields(before[:start]); len(words) > 0 {
		prev = strings.ToLower(words[len(words)-1])
	}

	var out []driver.Completion
	add := func(label, kind, detail string) {
		if strings.HasPrefix(strings.ToLower(label), prefix) {
			out = append(out, driver.Completion{Label: label, Kind: kind, Detail: detail})
		}
	}
	collections := func() {
		for _, name := range d.names() {
			add(name, "collection", fmt.Sprintf("%d rows", len(d[name].rows)))
		}
	}
	fields := func() {
		for _, name := range d.names() {
			for _, c := range d[name].cols {
				add(c.Name, "field", name+" "+c.NativeType)
			}
		}
	}
	switch prev {
	case "from", "reset":
		collections()
	case "where":
		fields()
	default:
		for _, k := range keywords {
			add(k, "keyword", "")
		}
		collections()
		fields()
	}
	return out
}
