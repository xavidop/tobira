// Package neo4j is the tobira driver for Neo4j, built on the official Go
// driver (github.com/neo4j/neo4j-go-driver/v5). Importing it registers the
// "neo4j" driver in driver.Default for the neo4j, neo4j+s, neo4j+ssc, bolt,
// bolt+s and bolt+ssc schemes.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/neo4j/
//
// Queries are Cypher. Namespaces are databases and collections are node
// labels (driver.KindGraph): Describe reports a label's properties from
// db.schema.nodeTypeProperties, with the element id as the key, and Browse
// pages through a label's nodes by element id with SKIP and LIMIT. Explain
// runs EXPLAIN, and PROFILE for an analyze. GraphReader walks a node's
// neighbourhood. Values that Cypher has and tobira lacks (nodes,
// relationships, paths, points, durations) come back as objects.
//
// Read-only is enforced by the server: every read-only call runs in a
// read-mode transaction, in which Neo4j refuses any write, schema change or
// write procedure, whatever the query says.
package neo4j
