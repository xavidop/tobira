//go:build integration

package neo4j

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// peopleSelect reads People back by property, in id order.
const peopleSelect = "MATCH (p:`tobira_people`) RETURN p.id AS id, p.name AS name, p.email AS email, p.age AS age, " +
	"p.score AS score, p.active AS active, p.created_at AS created_at, p.tags AS tags ORDER BY p.id"

// TestConformance runs the drivertest kit in strict mode. People are the
// nodes labelled tobira_people in the neo4j database; a label is the
// collection, and the element id its key, so People's id is a plain
// property.
func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsnFor(testPassword, ""), ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: dsnFor("definitely-wrong", "")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict: true,
		Fixture: drivertest.Fixture{
			Namespace:    testDB,
			Collection:   people,
			NoPrimaryKey: true, // the key is the element id; People's id is a property
			// Neo4j has no decimal and no JSON type: score is a float,
			// tags a list of strings.
			TypeOverrides: map[string]driver.Type{"score": driver.TypeFloat, "tags": driver.TypeList},
			SelectAll:     driver.Query{Text: peopleSelect},
			// A label that no node has matches nothing, as in Cypher; a
			// database that does not exist is not found.
			Missing:    driver.Query{Text: "RETURN 1", Namespace: "tobira_no_such_db"},
			Write:      driver.Query{Text: "MATCH (p:`tobira_people` {id: 1}) DETACH DELETE p"},
			ParamQuery: driver.Query{Text: "MATCH (p:`tobira_people`) WHERE p.id = $1 RETURN p.id AS id", Params: []driver.Value{driver.Int(7)}},
			CompoundWrites: []driver.Query{
				{Text: "MATCH (p:`tobira_people` {id: 1}) SET p.name = 'x' RETURN p.name"},
				{Text: "MERGE (x:`tobira_tmp` {k: 1}) RETURN x"},
				{Text: "FOREACH (i IN [1] | CREATE (:`tobira_tmp`)) RETURN 1"},
				{Text: "CALL { CREATE (x:`tobira_tmp`) RETURN x } RETURN x"},
				{Text: "CREATE INDEX tobira_tmp_idx IF NOT EXISTS FOR (n:`tobira_tmp`) ON (n.k)"},
				{Text: "CALL db.createLabel('tobira_tmp')"},
			},
			Slow:  driver.Query{Text: "UNWIND range(1, 400000000) AS x WITH x WHERE x % 7 = 0 RETURN count(x)"},
			Graph: &drivertest.GraphFixture{Query: driver.GraphQuery{Start: driver.Int(1), Depth: 1, Limit: 10}, MinNodes: 2},
		},
	})
}
