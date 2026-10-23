---
layout: ../../../layouts/DocsLayout.astro
title: Neo4j
---

# Neo4j

The `neo4j` driver connects tobira to Neo4j through the official Go driver. It is compiled into the
`tobira` binary and answers to the `neo4j://`, `neo4j+s://`, `neo4j+ssc://`, `bolt://`, `bolt+s://`
and `bolt+ssc://` schemes. You query in Cypher, browse the nodes of a label, explain and profile
statements, and walk a node's neighbourhood through the driver package.

## Quick start

```sh
tobira query "neo4j://neo4j@localhost/neo4j" \
  "MATCH (p:Person)-[:KNOWS]->(q) RETURN p.name, count(q) AS friends ORDER BY friends DESC" --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: graph
    driver: neo4j
    dsn: neo4j+s://reader@xxxxxxxx.databases.neo4j.io/neo4j
    password: env:GRAPH_NEO4J_PASSWORD
    read_only: true
```

## Connecting

```text
neo4j://localhost
neo4j://reader@graph.internal:7687/app?allow_plaintext=1
neo4j+s://reader@xxxxxxxx.databases.neo4j.io/neo4j
bolt+ssc://reader@graph.internal/app?timeout=30s
```

- The port defaults to 7687. One host: the `neo4j://` schemes discover a cluster from it, the
  `bolt://` schemes talk to that one server. IPv6 hosts go in brackets: `neo4j://[::1]:7687`.
- The path is the database (a name of letters, digits, `.` and `-`). Without one, the server's
  default database is used.
- **Password.** Keep it out of the DSN. A password from the connection's `password:` reference or
  `TOBIRA_CONN_<NAME>_PASSWORD` replaces the DSN's, and the DSN must then name the user (see
  [Passwords and secrets](/docs/secrets/)). Passwords never appear in errors.
- **TLS** is what the scheme says: `+s` encrypts and verifies the certificate, `+ssc` encrypts and
  trusts any certificate, a plain scheme does not encrypt. `tls=true` turns a plain scheme into
  its `+s` form.
- **Plain text.** Without TLS, a password goes to a host that is not this machine (`localhost` or a
  loopback address) only with `allow_plaintext=1`. Neo4j Aura (`*.neo4j.io`) only speaks TLS, so it
  is refused even then: use `neo4j+s://`.
- The connection's `options:` are refused, and so is any unknown parameter.

| Parameter | Default | Meaning |
| --- | --- | --- |
| `tls` | by the scheme | `true` turns a plain scheme into its `+s` form |
| `timeout` | the server's | per-query transaction timeout, such as `30s` |
| `connect_timeout` | `10s` | connect timeout |
| `allow_plaintext` | `false` | send a password without TLS to a host that is not this machine |

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, labels, describe | yes; databases are the namespaces, node labels the collections; describe lists a label's properties and their types |
| Browse | yes; a label's nodes by element id, with filters and sorting on the server |
| Explain | yes; `EXPLAIN` plans without running; analyze runs `PROFILE` |
| Graph neighbourhood | yes; the `GraphReader` capability of the [driver package](/docs/write-a-driver/) |
| Editing, transactions, completion, watching, native export | no |

## Queries

A query is one Cypher statement.

```cypher
MATCH (p:Person {name: 'Ada'})-[:KNOWS*1..2]-(q) RETURN DISTINCT q.name
MATCH (p:Person) WHERE p.born < 1900 RETURN p ORDER BY p.born LIMIT 20
CALL db.labels()
CREATE (p:Person {name: 'Grace'}) RETURN p
```

- Rows stream as the server sends them. A write or a schema change returns an empty result whose
  row count is what it changed: nodes and relationships created and deleted, plus properties set.
- Parameters are positional: the first binds to `$1`, the second to `$2`, and so on. A decimal is
  sent as a float (Neo4j has no exact decimal) and a JSON value as the map or list it holds.
- Running a query in another database than the connection's picks it as the namespace.

## Browsing

- **Databases** are listed from `SHOW DATABASES`, without `system`; the DSN's database is the
  default, or else the server's. A user who may not list databases sees the one the connection is
  on.
- **Labels** are the collections, each with its node count from the count store, up to 200
  labels. Relationship types are not listed: query them (`CALL db.relationshipTypes()`).
- **Describe** reads `db.schema.nodeTypeProperties`: every property any node with the label has,
  its Cypher types and whether every such node has it (else nullable), plus the indexes on the
  label. The element id is the key, under the `_id` column. The schema is kept for 30 seconds,
  since browsing asks for it on every page, and `db.schema.nodeTypeProperties` reads the whole
  graph.
- **Rows** are the element id and the label's properties; a node without a property shows NULL.
- **Filters**: every operator, on the server. Contains and starts with compare the property's
  text (`toString`). A raw filter is a Cypher `WHERE` body over the node variable `n`, such as
  `n.age > 60 AND n.name STARTS WITH 'A'`.
- **Paging** is by offset (`SKIP` and `LIMIT`), in element id order or the sort's; a page is 100
  rows by default, at most 10,000. A cursor is bound to its filter and sort.
- **Sorting** is done by the server. Neo4j puts NULLs last ascending and first descending.

## Graph neighbourhood

The `GraphReader` capability returns a node and the paths of up to a depth around it, in either
direction, as nodes with their labels and properties and relationships with their type, ends and
properties. The start is the node's element id, or the value of its `id` property. Labels keep
only the paths that end on a node with one of them. The depth is 1 by default and at most 6; the
limit is 100 paths by default and at most 1,000. The terminal UI has no graph view yet: the
capability is for hosts of the driver package and the AI tools.

## Read-only guard

`read_only: true` on a connection, or `--read-only` on one command, runs every statement in a
read-mode transaction, which the server enforces: a write, a schema change or a write procedure
fails with `read-only violation` and changes nothing, whatever the query says (`CREATE`, `MERGE`,
`SET`, `DELETE`, `FOREACH`, a `CALL { … }` subquery that writes, `CREATE INDEX`,
`db.createLabel`). A `PROFILE` of a write is refused the same way. Read-only does not limit cost:
a query may still read the whole graph.

## Types

| Cypher | tobira |
| --- | --- |
| `Integer` | int |
| `Float` | float |
| `String` | string |
| `Boolean` | bool |
| `ByteArray` | bytes |
| `DateTime`, `LocalDateTime`, `Date`, `Time`, `LocalTime` | time |
| `Duration` | string, ISO 8601 (`P1M2DT3S`) |
| `Point` | object (`srid`, `x`, `y` and `z`) |
| `List` | list |
| `Map` | object, keys in order |
| `Node` | object: `id` (the element id), `labels`, `properties` |
| `Relationship` | object: `id`, `type`, `start`, `end`, `properties` |
| `Path` | object: `nodes`, `relationships` |

A column is typed by its first row, since a Cypher result carries no types of its own; a column
whose first value is NULL shows as unknown.

## Limits

| Limit | Value |
| --- | --- |
| Browse page | 100 rows by default, at most 10,000 |
| Labels counted in a listing | 200; above that, counts are unknown |
| Neighbourhood depth | 6 |
| Neighbourhood paths | 100 by default, at most 1,000 |

## Compatibility

Tested with Neo4j 5.26 Community Edition, one server, with password authentication. Not yet
tested: Neo4j Aura, clusters and multiple databases (Enterprise Edition), TLS, and Neo4j 2025.x.
Memgraph and Amazon Neptune speak Bolt and Cypher but lack the `SHOW DATABASES` and
`db.schema.nodeTypeProperties` the listing and describe rely on; they are not supported.

## Troubleshooting

- **"the password would go to … unencrypted"**: use `neo4j+s://`. If the network is trusted, add
  `allow_plaintext=1` instead.
- **"… only speaks TLS: use the neo4j+s:// scheme"**: Neo4j Aura needs TLS.
- **"read-only violation"**: the statement writes, changes the schema or calls a write procedure,
  and the connection or the call is read-only.
- **A property is missing from the grid**: it was added after the schema was read; it appears
  within 30 seconds.
- **"Database does not exist"**: the DSN's path, or the namespace, names a database the server
  does not have; Community Edition has one, `neo4j`.
