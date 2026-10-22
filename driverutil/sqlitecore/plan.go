package sqlitecore

import (
	"strings"

	"github.com/xavidop/tobira/driver"
)

// MaxPlanDepth caps BuildPlan's tree depth and indentation.
const MaxPlanDepth = 64

// NoPlanRows and NoPlanRowsWrite are Plan.Text for a statement whose
// EXPLAIN QUERY PLAN has no rows. A write compiled to a direct write (INSERT
// … VALUES, an unfiltered DELETE) gets the second; anything else (a
// read-only PRAGMA, for one) the first.
const (
	NoPlanRows      = "(no plan rows)"
	NoPlanRowsWrite = "(no plan rows: the statement compiles to a direct write)"
)

// BuildPlan turns EXPLAIN QUERY PLAN rows (id, parent, notused, detail) for
// text into a Plan.
//
// A server picks the ids and parents, so the depth is capped at
// MaxPlanDepth: deeper rows attach to the node at that depth and keep its
// indentation, and the text stays linear in the row count.
func BuildPlan(rows []driver.Row, text string) *driver.Plan {
	root := &driver.PlanNode{Label: "QUERY PLAN", Rows: -1}
	nodes := map[int64]*driver.PlanNode{0: root}
	depth := map[int64]int{0: 0}
	capped := map[int64]*driver.PlanNode{0: root}
	var b strings.Builder
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		id, _ := r[0].AsInt()
		parent, _ := r[1].AsInt()
		detail := r[3].String()
		n := &driver.PlanNode{Label: detail, Rows: -1}
		p, ok := nodes[parent]
		if !ok {
			p, parent = root, 0
		}
		if depth[parent] >= MaxPlanDepth { // parent is at the cap: attach beside it
			p = capped[parent]
		}
		p.Children = append(p.Children, n)
		nodes[id] = n
		depth[id] = min(depth[parent]+1, MaxPlanDepth)
		capped[id] = n
		if depth[id] == MaxPlanDepth {
			capped[id] = p // children of id attach to its parent, at the cap
		}
		b.WriteString(strings.Repeat("  ", depth[id]-1))
		b.WriteString(detail)
		b.WriteByte('\n')
	}
	out := b.String()
	if out == "" {
		out = NoPlanRows
		if IsWriteStatement(text) {
			out = NoPlanRowsWrite
		}
	}
	return &driver.Plan{Format: "text", Text: out, Root: root}
}
