package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// notIterator is MySQL's FORMAT=TREE output for a statement the iterator
// executor does not run (single-table UPDATE and DELETE in 8.4).
const notIterator = "<not executable by iterator executor>"

// Explain returns the plan of q. The driver builds EXPLAIN FORMAT=JSON q,
// parsed into the plan tree, and EXPLAIN FORMAT=TREE q for Plan.Text (or the
// traditional table when the tree format has nothing to show). It never
// uses ANALYZE, so the statement is only planned, never run: Analyze is
// ErrUnsupported.
//
// The built statements must pass explainTarget on every connection: one
// SELECT, WITH, VALUES, TABLE, INSERT, REPLACE, UPDATE or DELETE statement,
// with no executable comment. They run through the same path as Query, so a
// read-only call (read-only connection or q.ReadOnly) needs a verified server
// and runs inside the read-only transaction with its post-check. MySQL
// refuses to explain INSERT, REPLACE, UPDATE and DELETE inside a read-only
// transaction (error 1792), so on a read-only call those are ErrReadOnly.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (_ *driver.Plan, err error) {
	defer func() { err = c.named(err) }()
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, errNoAnalyze()
	}
	ro := c.readOnly || q.ReadOnly
	check := explainCheck(ro)
	return explainPlan(c.syntaxFlavor(), func(format string) ([]driver.Column, []driver.Row, error) {
		text := explainText(format, q.Text)
		if err := check(text, lexMode{}); err != nil {
			return nil, nil, err
		}
		rs, err := c.run(ctx, driver.Query{Text: text, Params: q.Params, Namespace: q.Namespace}, ro, check)
		if err != nil {
			return nil, nil, err
		}
		cols := rs.Columns()
		rows, _, err := driver.Collect(ctx, rs, 0)
		return cols, rows, err
	})
}

var _ driver.Explainer = (*myTx)(nil)

// Explain returns the plan of q inside the transaction, on its connection,
// so it sees what the transaction made (temporary tables, uncommitted rows).
// The built statements pass the same explainTarget check as on the
// connection, run through Query's path (one at a time, with the post-check
// that the transaction is still open), and are only planned, never run:
// Analyze is ErrUnsupported. In a read-only Tx a refusal is ErrReadOnly, and
// MySQL refuses to explain INSERT, REPLACE, UPDATE and DELETE there (1792),
// as on a read-only connection. A per-query ReadOnly cannot be honoured in a
// read-write Tx, as in Query.
func (t *myTx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (_ *driver.Plan, err error) {
	defer func() { err = t.c.named(err) }()
	if opts.Analyze {
		return nil, errNoAnalyze()
	}
	check := explainCheck(t.readOnly)
	return explainPlan(t.c.syntaxFlavor(), func(format string) ([]driver.Column, []driver.Row, error) {
		rs, err := t.query(ctx, driver.Query{Text: explainText(format, q.Text), Params: q.Params, Namespace: q.Namespace, ReadOnly: q.ReadOnly}, check)
		if err != nil {
			return nil, nil, err
		}
		cols := rs.Columns()
		rows, _, err := driver.Collect(ctx, rs, 0)
		return cols, rows, err
	})
}

func errNoAnalyze() error {
	return driver.Wrap(driver.ErrUnsupported, Name, "explain", errors.New("EXPLAIN ANALYZE runs the statement; tobira's mysql driver only plans it"))
}

// explainCheck is the statement check of a built EXPLAIN: explainTarget, its
// refusal ErrReadOnly on a read-only path and ErrUnsupported otherwise.
func explainCheck(ro bool) checker {
	kind := driver.ErrUnsupported
	if ro {
		kind = driver.ErrReadOnly
	}
	return func(text string, mode lexMode) error {
		if err := explainTarget(text, mode); err != nil {
			return driver.Wrap(kind, Name, "explain", err)
		}
		return nil
	}
}

// explainPlan builds the Plan from explain, which runs the statement built
// by explainText for a format, in flavor f's syntax:
//
//   - MySQL: FORMAT=JSON for Plan.Root, and FORMAT=TREE for Plan.Text, or
//     the TRADITIONAL table when the tree has nothing to show;
//   - MariaDB (no TREE format): FORMAT=JSON and the TRADITIONAL table;
//   - SingleStore: EXPLAIN JSON for Plan.Root and EXPLAIN's text lines.
func explainPlan(f flavor, explain func(format string) ([]driver.Column, []driver.Row, error)) (*driver.Plan, error) {
	if f == flavorSingleStore {
		return singleStorePlan(explain)
	}
	_, rows, err := explain(fmtJSON)
	if err != nil {
		return nil, err
	}
	raw := firstCell(rows)
	root, err := parsePlan(raw)
	if err != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "explain", err)
	}
	if f == flavorMySQL {
		_, rows, err = explain(fmtTree)
		if err != nil {
			return nil, err
		}
		if tree := firstCell(rows); tree != "" && !strings.Contains(tree, notIterator) {
			return &driver.Plan{Format: "text", Text: strings.TrimRight(tree, "\n"), Root: root}, nil
		}
	}
	cols, rows, err := explain(fmtTraditional)
	if err != nil {
		return nil, err
	}
	return &driver.Plan{Format: "text", Text: renderTable(cols, rows), Root: root}, nil
}

// singleStorePlan is explainPlan for SingleStore: EXPLAIN JSON parsed into
// the tree, and the lines of plain EXPLAIN as the text.
func singleStorePlan(explain func(format string) ([]driver.Column, []driver.Row, error)) (*driver.Plan, error) {
	_, rows, err := explain(fmtBareJSON)
	if err != nil {
		return nil, err
	}
	root, err := parseSingleStorePlan(firstCell(rows))
	if err != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "explain", err)
	}
	_, rows, err = explain(fmtBare)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 && !r[0].IsNull() {
			lines = append(lines, r[0].String())
		}
	}
	return &driver.Plan{Format: "text", Text: strings.TrimRight(strings.Join(lines, "\n"), "\n"), Root: root}, nil
}

// firstCell returns the text of the single cell of a FORMAT=JSON or
// FORMAT=TREE result.
func firstCell(rows []driver.Row) string {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return ""
	}
	if b, ok := rows[0][0].AsJSON(); ok {
		return string(b)
	}
	return rows[0][0].String()
}

// renderTable lays out the traditional EXPLAIN result as an aligned table.
func renderTable(cols []driver.Column, rows []driver.Row) string {
	cells := make([][]string, 0, len(rows)+1)
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = c.Name
	}
	cells = append(cells, head)
	for _, r := range rows {
		line := make([]string, len(r))
		for i, v := range r {
			line[i] = "NULL"
			if !v.IsNull() {
				line[i] = v.String()
			}
		}
		cells = append(cells, line)
	}
	width := make([]int, len(cols))
	for _, line := range cells {
		for i, s := range line {
			if i < len(width) {
				width[i] = max(width[i], len([]rune(s)))
			}
		}
	}
	var b strings.Builder
	for n, line := range cells {
		if n > 0 {
			b.WriteByte('\n')
		}
		for i, s := range line {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(s)
			if i < len(line)-1 && i < len(width) {
				b.WriteString(strings.Repeat(" ", width[i]-len([]rune(s))))
			}
		}
	}
	return b.String()
}

// planSkip are the object-valued keys of a FORMAT=JSON plan that are
// details of their node rather than plan steps.
var planSkip = map[string]bool{"cost_info": true}

// parsePlan turns EXPLAIN FORMAT=JSON output into a PlanNode tree. The root
// is the query_block (or whatever single object the document holds); every
// nested object or array of objects (a table, nested_loop, an
// ordering_operation, a subquery …) becomes a child labelled with its key.
func parsePlan(raw string) (*driver.PlanNode, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("cannot parse the plan: %w", err)
	}
	if len(doc) == 0 {
		return nil, errors.New("cannot parse the plan: empty result")
	}
	if len(doc) == 1 {
		for k, v := range doc {
			if obj, ok := v.(map[string]any); ok {
				return planNode(k, obj), nil
			}
		}
	}
	return planNode("plan", doc), nil
}

func planNode(label string, obj map[string]any) *driver.PlanNode {
	n := &driver.PlanNode{Label: label, Rows: -1}
	var detail []string
	for _, k := range []string{"table_name", "access_type", "key", "message"} {
		if s, ok := obj[k].(string); ok && s != "" {
			detail = append(detail, s)
		}
	}
	n.Detail = strings.Join(detail, " ")
	if ci, ok := obj["cost_info"].(map[string]any); ok {
		for _, k := range []string{"query_cost", "prefix_cost", "sort_cost"} {
			if f, ok := number(ci[k]); ok {
				n.Cost = f
				break
			}
		}
	} else if f, ok := number(obj["cost"]); ok { // MariaDB
		n.Cost = f
	}
	for _, k := range []string{"rows_examined_per_scan", "rows_produced_per_join", "rows"} { // rows: MariaDB
		if f, ok := number(obj[k]); ok {
			n.Rows = int64(f)
			break
		}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if planSkip[k] {
			continue
		}
		switch v := obj[k].(type) {
		case map[string]any:
			n.Children = append(n.Children, planNode(k, v))
		case []any:
			for _, e := range v {
				child, ok := e.(map[string]any)
				if !ok {
					continue
				}
				// nested_loop: [{"table": {…}}, …] — unwrap the one-key wrapper.
				if len(child) == 1 {
					for ck, cv := range child {
						if inner, ok := cv.(map[string]any); ok {
							n.Children = append(n.Children, planNode(ck, inner))
							child = nil
						}
					}
				}
				if child != nil {
					n.Children = append(n.Children, planNode(k, child))
				}
			}
		}
	}
	return n
}

// number reads a JSON number, or a number spelled as a string ("1.00").
func number(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

// parseSingleStorePlan turns SingleStore's EXPLAIN JSON output into a
// PlanNode tree. The document's "explain" array holds the plan's operators;
// each is labelled with its executor (Gather, Project, ColumnStoreScan …),
// with its table, index and condition as detail and its "inputs" and
// "subselects" as children. Several top operators get a "plan" root.
func parseSingleStorePlan(raw string) (*driver.PlanNode, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("cannot parse the plan: %w", err)
	}
	ops, _ := doc["explain"].([]any)
	var nodes []*driver.PlanNode
	for _, op := range ops {
		if obj, ok := op.(map[string]any); ok {
			nodes = append(nodes, singleStoreNode(obj))
		}
	}
	switch len(nodes) {
	case 0:
		return nil, errors.New("cannot parse the plan: no operators")
	case 1:
		return nodes[0], nil
	}
	return &driver.PlanNode{Label: "plan", Rows: -1, Children: nodes}, nil
}

func singleStoreNode(obj map[string]any) *driver.PlanNode {
	n := &driver.PlanNode{Label: "operator", Rows: -1}
	if s, ok := obj["executor"].(string); ok && s != "" {
		n.Label = s
	}
	var detail []string
	table, _ := obj["table"].(string)
	if db, _ := obj["db"].(string); db != "" && table != "" {
		table = db + "." + table
	}
	if table != "" {
		detail = append(detail, table)
	}
	if s, ok := obj["index"].(string); ok && s != "" {
		detail = append(detail, s)
	}
	if conds, ok := obj["condition"].([]any); ok {
		var cs []string
		for _, c := range conds {
			if s, ok := c.(string); ok && strings.TrimSpace(s) != "" {
				cs = append(cs, strings.TrimSpace(s))
			}
		}
		if len(cs) > 0 {
			detail = append(detail, strings.Join(cs, ", "))
		}
	}
	n.Detail = strings.Join(detail, " ")
	if f, ok := number(obj["est_rows"]); ok {
		n.Rows = int64(f)
	}
	for _, k := range []string{"inputs", "subselects"} {
		children, _ := obj[k].([]any)
		for _, c := range children {
			if child, ok := c.(map[string]any); ok {
				n.Children = append(n.Children, singleStoreNode(child))
			}
		}
	}
	return n
}
