# tobira Neo4j driver

The Neo4j driver for [tobira](https://github.com/xavidop/tobira), built on
the official Go driver (`github.com/neo4j/neo4j-go-driver/v5`). It registers
the `neo4j` driver for the `neo4j://`, `neo4j+s://`, `neo4j+ssc://`,
`bolt://`, `bolt+s://` and `bolt+ssc://` schemes and is compiled into the
`tobira` binary.

## Connect

```text
neo4j://localhost
neo4j://reader@graph.internal:7687/app?allow_plaintext=1
neo4j+s://reader@xxxxxxxx.databases.neo4j.io/neo4j
bolt+ssc://reader@graph.internal/app?timeout=30s
```

The port defaults to 7687 and the path is the database. Parameters: `tls`,
`timeout`, `connect_timeout` and `allow_plaintext`. Without TLS, a password
goes to a host that is not this machine only with `allow_plaintext=1`, and
never to Neo4j Aura.

## Quick example

Queries are Cypher.

```sh
tobira query "neo4j://neo4j@localhost/neo4j" \
  "MATCH (p:Person)-[:KNOWS]->(q) RETURN p.name, count(q) AS friends" --read-only
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, labels, describe | yes; node labels are the collections; describe lists a label's properties |
| Browse | yes; a label's nodes by element id, filters and sorting on the server |
| Explain | yes; `EXPLAIN`, and `PROFILE` for an analyze |
| Graph neighbourhood | yes; `GraphReader`: a node and the paths around it |
| Read-only | yes; read-mode transactions, enforced by the server |
| Editing, transactions, completion, watching, native export | no |

The full reference is
[xavidop.github.io/tobira/docs/drivers/neo4j](https://xavidop.github.io/tobira/docs/drivers/neo4j/).

## Tests

`go test ./...` runs the unit tests. `go test -tags integration ./...`
starts Neo4j 5.26 with testcontainers and runs the conformance kit in strict
mode, the neighbourhood, browse, describe and explain checks, and the
read-only probes.
