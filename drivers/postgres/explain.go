package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// Explain returns the plan of q. It runs EXPLAIN through the same path as
// Query, so the read-only transaction, the single-statement rule and the error
// mapping apply (a q.Params value is bound as a parameter).
//
// Without Analyze the statement is only planned, never run, and Plan.Text is
// the output of EXPLAIN (FORMAT TEXT) next to the tree parsed from
// EXPLAIN (FORMAT JSON).
//
// EXPLAIN ANALYZE executes the statement, so it always runs in a transaction
// that is rolled back, even on a writable connection: explaining never
// persists side effects. On a read-only path a statement that writes fails
// with ErrReadOnly. The statement runs once; Plan.Format is "json" and
// Plan.Text is the indented JSON, because asking for a second, text-format
// run would execute it twice.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (_ *driver.Plan, err error) {
	defer func() { err = c.named(err) }()
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	return c.explain(ctx, q, opts, nil)
}

var _ driver.Explainer = (*pgTx)(nil)

// Explain returns the plan of q inside the transaction, on its connection, so
// it sees what the transaction made. It goes through the same path and guards
// as the connection's Explain, with the transaction's access mode (a read-only
// Tx refuses a write, COPY and the server-file functions; a per-query
// ReadOnly cannot be honoured in a read-write Tx, as in Query).
//
// EXPLAIN runs inside a savepoint that is rolled back afterwards, so what
// EXPLAIN ANALYZE executed never persists, not even in the transaction, and a
// failed EXPLAIN does not abort the transaction.
func (t *pgTx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (_ *driver.Plan, err error) {
	defer func() { err = t.c.named(err) }()
	const op = "explain"
	tx, err := t.live(op)
	if err != nil {
		return nil, err
	}
	if q.ReadOnly && !t.readOnly {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a read-only query cannot run inside a read-write transaction: begin a read-only one"))
	}
	sp := savepointPrefix + randomHex()
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		return nil, mapError(ctx, op, err)
	}
	plan, err := t.c.explain(ctx, q, opts, &txScope{tx: tx, readOnly: t.readOnly, onEscape: t.kill})
	if _, lerr := t.live(op); lerr != nil {
		return nil, errors.Join(err, lerr) // a statement ended the transaction
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if _, rerr := tx.Exec(rctx, "ROLLBACK TO SAVEPOINT "+sp); rerr != nil && err == nil {
		return nil, mapError(ctx, op, rerr)
	}
	_, _ = tx.Exec(rctx, "RELEASE SAVEPOINT "+sp)
	return plan, err
}

// explain is Explain on the pool (scope nil) or inside a caller's Tx.
func (c *conn) explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions, scope *txScope) (*driver.Plan, error) {
	const op = "explain"
	if c.flavor.textExplain() {
		return c.explainText(ctx, q, opts, scope)
	}
	options := "FORMAT JSON"
	if opts.Analyze {
		options = "ANALYZE, FORMAT JSON"
	}
	rows, err := c.explainRows(ctx, q, options, opts.Analyze, scope)
	if err != nil {
		return nil, err
	}
	root, err := parsePlan(rows)
	if err != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, err)
	}
	if opts.Analyze {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, rawPlanJSON(rows), "", "  "); err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, err)
		}
		return &driver.Plan{Format: "json", Text: pretty.String(), Root: root}, nil
	}
	lines, err := c.explainRows(ctx, q, "FORMAT TEXT", false, scope)
	if err != nil {
		return nil, err
	}
	text := make([]string, 0, len(lines))
	for _, r := range lines {
		if len(r) > 0 {
			text = append(text, r[0].String())
		}
	}
	return &driver.Plan{Format: "text", Text: strings.Join(text, "\n"), Root: root}, nil
}

// explainText is explain for the flavors whose EXPLAIN only prints text
// (CockroachDB, Redshift): one run of EXPLAIN [ANALYZE], whose lines are
// Plan.Text and parse into the tree. EXPLAIN ANALYZE runs, like the JSON
// path, in a transaction that is rolled back.
func (c *conn) explainText(ctx context.Context, q driver.Query, opts driver.ExplainOptions, scope *txScope) (*driver.Plan, error) {
	prefix, err := c.flavor.textExplainPrefix(opts.Analyze)
	if err != nil {
		return nil, err
	}
	rows, err := c.explainStatement(ctx, q, prefix, opts.Analyze, scope)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			lines = append(lines, r[0].String())
		}
	}
	return &driver.Plan{Format: "text", Text: strings.Join(lines, "\n"), Root: parseTextPlan(lines)}, nil
}

// explainRows runs EXPLAIN (options) over q.Text and collects the rows.
func (c *conn) explainRows(ctx context.Context, q driver.Query, options string, discard bool, scope *txScope) ([]driver.Row, error) {
	return c.explainStatement(ctx, q, "EXPLAIN ("+options+") ", discard, scope)
}

// explainStatement runs prefix + q.Text and collects the rows.
func (c *conn) explainStatement(ctx context.Context, q driver.Query, prefix string, discard bool, scope *txScope) ([]driver.Row, error) {
	rs, err := c.query(ctx, driver.Query{
		Text:      prefix + q.Text,
		Params:    q.Params,
		Namespace: q.Namespace,
		ReadOnly:  q.ReadOnly,
	}, discard, scope)
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	return rows, err
}

// rawPlanJSON returns the JSON document in the single cell of a FORMAT JSON
// explain result.
func rawPlanJSON(rows []driver.Row) []byte {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil
	}
	if b, ok := rows[0][0].AsJSON(); ok {
		return b
	}
	return []byte(rows[0][0].String())
}

type planJSON struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	IndexName    string     `json:"Index Name"`
	JoinType     string     `json:"Join Type"`
	TotalCost    float64    `json:"Total Cost"`
	PlanRows     *float64   `json:"Plan Rows"`
	Plans        []planJSON `json:"Plans"`
}

// parsePlan turns EXPLAIN (FORMAT JSON) output into a PlanNode tree.
func parsePlan(rows []driver.Row) (*driver.PlanNode, error) {
	var doc []struct {
		Plan planJSON `json:"Plan"`
	}
	if err := json.Unmarshal(rawPlanJSON(rows), &doc); err != nil {
		return nil, fmt.Errorf("cannot parse the plan: %w", err)
	}
	if len(doc) == 0 {
		return nil, errors.New("cannot parse the plan: empty result")
	}
	return planNode(doc[0].Plan), nil
}

func planNode(p planJSON) *driver.PlanNode {
	var detail []string
	for _, s := range []string{p.RelationName, p.IndexName, p.JoinType} {
		if s != "" {
			detail = append(detail, s)
		}
	}
	n := &driver.PlanNode{Label: p.NodeType, Detail: strings.Join(detail, " "), Cost: p.TotalCost, Rows: -1}
	if p.PlanRows != nil {
		n.Rows = int64(*p.PlanRows)
	}
	for _, c := range p.Plans {
		n.Children = append(n.Children, planNode(c))
	}
	return n
}

// costRows matches the "(cost=a..b rows=n" of a PostgreSQL text plan line.
var costRows = regexp.MustCompile(`\s+\(cost=[\d.]+\.\.([\d.]+) rows=(\d+)`)

// parseTextPlan turns the lines of a text EXPLAIN into a PlanNode tree, or
// nil when no node is found. It reads two layouts:
//
//   - PostgreSQL's (and Redshift's): a node is a line with "(cost=…)", a
//     child starts with "->", and the other lines (conditions, filters) are
//     the details of the node above them;
//   - CockroachDB's: a node is a line with "• name", drawn as a tree, whose
//     "table:" and "estimated row count:" attributes follow it.
//
// The depth of a node is the column of its label, so a node's parent is the
// nearest node above it that starts further left.
func parseTextPlan(lines []string) *driver.PlanNode {
	crdb := slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "• ") })
	type open struct {
		col  int
		node *driver.PlanNode
	}
	var root *driver.PlanNode
	var stack []open
	var details []string // of the newest node, PostgreSQL layout
	flush := func() {
		if len(stack) > 0 && len(details) > 0 {
			stack[len(stack)-1].node.Detail = strings.Join(details, "; ")
		}
		details = nil
	}
	push := func(col int, n *driver.PlanNode) {
		flush()
		for len(stack) > 0 && stack[len(stack)-1].col >= col {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			if root != nil {
				return // a second top-level node: not a tree we understand
			}
			root = n
		} else {
			parent := stack[len(stack)-1].node
			parent.Children = append(parent.Children, n)
		}
		stack = append(stack, open{col, n})
	}
	for _, line := range lines {
		if crdb {
			if i := strings.Index(line, "• "); i >= 0 {
				push(utf8.RuneCountInString(line[:i]), &driver.PlanNode{Label: strings.TrimSpace(line[i+len("• "):]), Rows: -1})
				continue
			}
			if len(stack) == 0 {
				continue // the header: distribution, vectorized
			}
			attr := strings.TrimLeft(line, " │├└─")
			n := stack[len(stack)-1].node
			if v, ok := strings.CutPrefix(attr, "table: "); ok {
				n.Detail = v
			} else if v, ok := strings.CutPrefix(attr, "estimated row count: "); ok {
				num, _, _ := strings.Cut(v, " ")
				if rows, err := strconv.ParseInt(strings.ReplaceAll(num, ",", ""), 10, 64); err == nil {
					n.Rows = rows
				}
			}
			continue
		}
		m := costRows.FindStringSubmatchIndex(line)
		if m == nil {
			if t := strings.TrimSpace(line); t != "" && len(stack) > 0 {
				details = append(details, t)
			}
			continue
		}
		head := line[:m[0]]
		col := len(head) - len(strings.TrimLeft(head, " "))
		label := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(head), "->"))
		n := &driver.PlanNode{Label: label, Rows: -1}
		n.Cost, _ = strconv.ParseFloat(line[m[2]:m[3]], 64)
		if rows, err := strconv.ParseInt(line[m[4]:m[5]], 10, 64); err == nil {
			n.Rows = rows
		}
		push(col, n)
	}
	flush()
	return root
}
