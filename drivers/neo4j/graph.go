package neo4j

import (
	"context"
	"errors"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/xavidop/tobira/driver"
)

const (
	defaultNeighborLimit = 100
	maxNeighborLimit     = 1000
	maxNeighborDepth     = 6
)

// Neighbors returns a node and its neighbourhood: every path of up to
// Depth relationships (1 by default, at most 6) from it, in any direction,
// at most Limit paths (100 by default, 1000 at most). Start is the node's
// element id as a String, or the value of its id property (a String or an
// Int). Labels, when given, keep only the paths that end on a node with
// one of them. A start that matches no node is ErrNotFound.
func (c *conn) Neighbors(ctx context.Context, q driver.GraphQuery) (*driver.Graph, error) {
	const op = "neighbors"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	depth := q.Depth
	if depth <= 0 {
		depth = 1
	}
	if depth > maxNeighborDepth {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("depth %d: at most %d", depth, maxNeighborDepth))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultNeighborLimit
	}
	limit = min(limit, maxNeighborLimit)
	var pred string
	switch q.Start.Type() {
	case driver.TypeString:
		pred = "elementId(s) = $start OR s.id = $start"
	case driver.TypeInt:
		pred = "s.id = $start"
	default:
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the start must be an element id or an id property value (a string or an integer)"))
	}
	start, _ := encode(q.Start)
	labels := make([]any, 0, len(q.Labels))
	for _, l := range q.Labels {
		labels = append(labels, l)
	}
	cypher := fmt.Sprintf(
		"MATCH (s) WHERE %s WITH s LIMIT 1 "+
			"OPTIONAL MATCH p = (s)-[*1..%d]-(m) WHERE size($labels) = 0 OR any(l IN labels(m) WHERE l IN $labels) "+
			"WITH s, p LIMIT $limit RETURN s, collect(p) AS paths", pred, depth)
	vals, err := c.one(ctx, op, "", cypher, map[string]any{"start": start, "labels": labels, "limit": limit})
	if err != nil {
		if errors.Is(err, errNoRows) {
			return nil, driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no node matches Start"))
		}
		return nil, err
	}
	g := &driver.Graph{}
	nodes, edges := map[string]bool{}, map[string]bool{}
	addNode := func(n dbtype.Node) {
		if nodes[n.ElementId] {
			return
		}
		nodes[n.ElementId] = true
		props, _ := mapValue(n.Props).AsObject()
		g.Nodes = append(g.Nodes, driver.Node{ID: n.ElementId, Labels: n.Labels, Props: props})
	}
	addEdge := func(r dbtype.Relationship) {
		if edges[r.ElementId] {
			return
		}
		edges[r.ElementId] = true
		props, _ := mapValue(r.Props).AsObject()
		g.Edges = append(g.Edges, driver.Edge{ID: r.ElementId, From: r.StartElementId, To: r.EndElementId, Type: r.Type, Props: props})
	}
	if s, ok := vals[0].(dbtype.Node); ok {
		addNode(s)
	}
	if paths, ok := vals[1].([]any); ok {
		for _, p := range paths {
			path, ok := p.(dbtype.Path)
			if !ok {
				continue
			}
			for _, n := range path.Nodes {
				addNode(n)
			}
			for _, r := range path.Relationships {
				addEdge(r)
			}
		}
	}
	return g, nil
}
