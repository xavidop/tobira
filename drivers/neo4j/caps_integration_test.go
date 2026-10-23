//go:build integration

package neo4j

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestNeighbors(t *testing.T) {
	c := openTest(t, false)
	g, _ := driver.As[driver.GraphReader](c)
	ctx := context.Background()
	graph, err := g.Neighbors(ctx, driver.GraphQuery{Start: driver.Int(2), Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Person 2 knows 3 and is known by 1.
	if len(graph.Nodes) != 3 || len(graph.Edges) != 2 {
		t.Fatalf("depth 1 around id 2: %d nodes, %d edges", len(graph.Nodes), len(graph.Edges))
	}
	if graph.Nodes[0].Labels[0] != people || graph.Edges[0].Type != "KNOWS" {
		t.Fatalf("graph = %+v", graph)
	}
	if v, ok := graph.Nodes[0].Props.Get("id"); !ok || !driver.Value.IsNull(v) && v.String() != "2" {
		t.Fatalf("the start node is not first or has no id: %+v", graph.Nodes[0])
	}
	// The start node's element id works as Start too.
	again, err := g.Neighbors(ctx, driver.GraphQuery{Start: driver.String(graph.Nodes[0].ID), Depth: 2, Limit: 1000})
	if err != nil || len(again.Nodes) != 4 {
		t.Fatalf("depth 2 by element id: %d nodes, %v", len(again.Nodes), err)
	}
	// Labels keep only paths ending on a matching node.
	none, err := g.Neighbors(ctx, driver.GraphQuery{Start: driver.Int(2), Labels: []string{"tobira_team"}})
	if err != nil || len(none.Nodes) != 1 || len(none.Edges) != 0 {
		t.Fatalf("label filter: %+v, %v", none, err)
	}
	if _, err := g.Neighbors(ctx, driver.GraphQuery{Start: driver.Int(999999)}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("unknown start: %v", err)
	}
	if _, err := g.Neighbors(ctx, driver.GraphQuery{Start: driver.Int(1), Depth: 99}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("depth 99: %v", err)
	}
}

func TestBrowseFiltersAndSort(t *testing.T) {
	c := openTest(t, false)
	b, _ := driver.As[driver.Browser](c)
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: testDB, Name: people}
	rs, err := b.Browse(ctx, ref, driver.BrowseOptions{Filter: driver.Filter{Raw: "n.age > 60"}, Sort: []driver.SortKey{{Field: "name"}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) == 0 || st.NextCursor != "" {
		t.Fatalf("raw filter: %d rows, %+v, %v", len(rows), st, err)
	}
	cols := rs.Columns()
	if cols[0].Name != idColumn || !cols[0].Key || cols[0].Source == nil || cols[0].Source.Name != people {
		t.Fatalf("columns = %+v", cols)
	}
	_, err = b.Browse(ctx, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "nope", Op: driver.OpEq, Value: driver.Int(1)}}}})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Fatalf("unknown property: %v", err)
	}
	first, err := b.Browse(ctx, ref, driver.BrowseOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	_, st, _ = driver.Collect(ctx, first, 0)
	if _, err := b.Browse(ctx, ref, driver.BrowseOptions{Limit: 10, Cursor: st.NextCursor, Sort: []driver.SortKey{{Field: "age"}}}); !errors.Is(err, driver.ErrSyntax) {
		t.Fatalf("a cursor replayed with another sort: %v", err)
	}
	if _, err := b.Browse(ctx, driver.CollectionRef{Namespace: testDB, Name: "tobira_nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("unknown label: %v", err)
	}
}

func TestDescribeFields(t *testing.T) {
	c := openTest(t, false)
	sch, err := c.Describe(context.Background(), driver.CollectionRef{Namespace: testDB, Name: people})
	if err != nil {
		t.Fatal(err)
	}
	if sch.Fields[0].Name != idColumn || sch.PrimaryKey[0] != idColumn || !sch.Schemaless || sch.RowEstimate != 57 {
		t.Fatalf("schema = %+v", sch)
	}
	byName := map[string]driver.Field{}
	for _, f := range sch.Fields {
		byName[f.Name] = f
	}
	if f := byName["age"]; !f.Nullable || f.NativeType != "Long" {
		t.Errorf("age = %+v, want a nullable Long", f)
	}
	if f := byName["name"]; f.Nullable || f.Type != driver.TypeString {
		t.Errorf("name = %+v, want a mandatory String", f)
	}
	if f := byName["tags"]; f.Type != driver.TypeList || f.NativeType != "StringArray" {
		t.Errorf("tags = %+v, want a StringArray list", f)
	}
}

func TestNamespacesAndNodeValues(t *testing.T) {
	c := openTest(t, false)
	ctx := context.Background()
	nss, err := c.Namespaces(ctx)
	if err != nil || len(nss) == 0 {
		t.Fatalf("namespaces = %+v, %v", nss, err)
	}
	for _, ns := range nss {
		if ns.Name == "system" || ns.Label != "database" || (ns.Name == testDB) != ns.Default {
			t.Fatalf("namespace %+v", ns)
		}
	}
	rs, err := c.Query(ctx, driver.Query{Text: "MATCH (p:`tobira_people` {id: 1})-[r:KNOWS]->(q) RETURN p, r LIMIT 1"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if cols := rs.Columns(); cols[0].NativeType != "Node" || cols[1].NativeType != "Relationship" || cols[0].Type != driver.TypeObject {
		t.Fatalf("columns = %+v", cols)
	}
	node, _ := rows[0][0].AsObject()
	if labels, _ := node.Get("labels"); labels.String() != `["tobira_people"]` {
		t.Fatalf("node = %s", rows[0][0])
	}
	// A write counts what it changed; a read reports -1.
	rs, err = c.Query(ctx, driver.Query{Text: "CREATE (x:`tobira_probe` {k: 1}) WITH x MATCH (x) DELETE x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, st, err := driver.Collect(ctx, rs, 0); err != nil || st.RowsAffected != 3 {
		t.Fatalf("write stats = %+v, %v", st, err)
	}
}

func TestExplainAndProfile(t *testing.T) {
	c := openTest(t, false)
	x, _ := driver.As[driver.Explainer](c)
	ctx := context.Background()
	plan, err := x.Explain(ctx, driver.Query{Text: "EXPLAIN " + peopleSelect}, driver.ExplainOptions{})
	if err != nil || plan.Root == nil || !strings.Contains(plan.Text, "NodeByLabelScan") {
		t.Fatalf("explain = %+v, %v", plan, err)
	}
	prof, err := x.Explain(ctx, driver.Query{Text: peopleSelect}, driver.ExplainOptions{Analyze: true})
	if err != nil || !strings.Contains(prof.Text, "db hits=") {
		t.Fatalf("profile = %+v, %v", prof, err)
	}
	_, err = x.Explain(ctx, driver.Query{Text: "CREATE (:`tobira_probe`)", ReadOnly: true}, driver.ExplainOptions{Analyze: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("profile of a write on a read-only call: %v", err)
	}
}

func TestReadOnlyConnRefusesProcedureWrites(t *testing.T) {
	ro := openTest(t, true)
	_, err := ro.Query(context.Background(), driver.Query{Text: "CALL db.createLabel('tobira_tmp2')"})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("write procedure on a read-only conn: %v", err)
	}
}
