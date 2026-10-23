package neo4j

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/xavidop/tobira/driver"
)

// Explain returns the planner's tree for a statement: EXPLAIN, which runs
// nothing, or with Analyze PROFILE, which runs it and reports the rows
// and database hits of every operator. A PROFILE of a write on a
// read-only call fails as the write would: the server refuses it.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	ps, err := params(q.Params)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	text := stripPrefix(strings.TrimSpace(q.Text))
	prefix := "EXPLAIN "
	if opts.Analyze {
		prefix = "PROFILE "
	}
	sess := c.session(ctx, q.Namespace, q.ReadOnly)
	defer func() { _ = sess.Close(context.WithoutCancel(ctx)) }()
	res, err := sess.Run(ctx, prefix+text, ps)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	sum, err := res.Consume(ctx)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	var root *driver.PlanNode
	if opts.Analyze {
		root = profiledNode(sum.Profile())
	} else {
		root = planNode(sum.Plan())
	}
	if root == nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("the server returned no plan"))
	}
	var sb strings.Builder
	renderPlan(&sb, root, 0)
	return &driver.Plan{Format: "text", Text: strings.TrimRight(sb.String(), "\n"), Root: root}, nil
}

// stripPrefix drops a leading EXPLAIN or PROFILE, which the caller may have
// typed, so the option decides which runs.
func stripPrefix(text string) string {
	for _, p := range []string{"EXPLAIN", "PROFILE"} {
		if len(text) > len(p) && strings.EqualFold(text[:len(p)], p) && (text[len(p)] == ' ' || text[len(p)] == '\n' || text[len(p)] == '\t') {
			return strings.TrimSpace(text[len(p):])
		}
	}
	return text
}

func planNode(p neo4j.Plan) *driver.PlanNode {
	if p == nil {
		return nil
	}
	n := &driver.PlanNode{Label: p.Operator(), Detail: planDetail(p.Arguments(), p.Identifiers()), Rows: -1}
	if v, ok := p.Arguments()["EstimatedRows"].(float64); ok {
		n.Rows = int64(v)
	}
	for _, ch := range p.Children() {
		if c := planNode(ch); c != nil {
			n.Children = append(n.Children, c)
		}
	}
	return n
}

func profiledNode(p neo4j.ProfiledPlan) *driver.PlanNode {
	if p == nil {
		return nil
	}
	n := &driver.PlanNode{Label: p.Operator(), Detail: planDetail(p.Arguments(), p.Identifiers()), Rows: p.Records(), Cost: float64(p.DbHits())}
	for _, ch := range p.Children() {
		if c := profiledNode(ch); c != nil {
			n.Children = append(n.Children, c)
		}
	}
	return n
}

// planDetail is the operator's Details argument (what it matches or
// filters) and the identifiers it introduces.
func planDetail(args map[string]any, ids []string) string {
	var parts []string
	if d, ok := args["Details"].(string); ok && d != "" {
		parts = append(parts, d)
	}
	if len(ids) > 0 {
		ids = slices.Clone(ids)
		slices.Sort(ids)
		parts = append(parts, "("+strings.Join(ids, ", ")+")")
	}
	return strings.Join(parts, " ")
}

func renderPlan(sb *strings.Builder, n *driver.PlanNode, depth int) {
	sb.WriteString(strings.Repeat("  ", depth) + n.Label)
	if n.Detail != "" {
		sb.WriteString(" " + n.Detail)
	}
	var stats []string
	if n.Rows >= 0 {
		stats = append(stats, fmt.Sprintf("rows=%d", n.Rows))
	}
	if n.Cost > 0 {
		stats = append(stats, fmt.Sprintf("db hits=%.0f", n.Cost))
	}
	if len(stats) > 0 {
		sb.WriteString(" [" + strings.Join(stats, " ") + "]")
	}
	sb.WriteString("\n")
	for _, c := range n.Children {
		renderPlan(sb, c, depth+1)
	}
}
