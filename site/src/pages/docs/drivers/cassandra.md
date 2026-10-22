---
layout: ../../../layouts/DocsLayout.astro
title: Cassandra
---

# Cassandra

The `cassandra` driver connects tobira to Apache Cassandra and ScyllaDB through the Apache gocql
driver. It is compiled into the `tobira` binary and answers to the `cassandra://` and `scylla://`
schemes. You query in CQL, browse tables in token order and edit one row at a time.

## Quick start

```sh
tobira query "cassandra://reader@localhost/shop" \
  "SELECT * FROM orders WHERE tenant = 'acme'" --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: events
    driver: cassandra
    dsn: cassandra://reader@cass1,cass2/shop?dc=eu-west&tls=true
    password: env:EVENTS_CASSANDRA_PASSWORD
    read_only: true
```

## Connecting

```text
cassandra://localhost/shop
cassandra://reader@cass1:9042,cass2:9042/shop?consistency=LOCAL_ONE&dc=eu-west
cassandra://reader@cass.example.com/shop?tls=true&tls_ca=/etc/ssl/cass-ca.pem
scylla://reader@scylla1/shop?proto=4
```

- The port defaults to 9042. Separate several hosts with commas. IPv6 hosts go in brackets:
  `cassandra://[::1]:9042/shop`.
- The path is the default keyspace (letters, digits and `_`). Without one, qualify table names
  (`shop.orders`).
- **Password.** Keep it out of the DSN. A password from the connection's `password:` reference or
  `TOBIRA_CONN_<NAME>_PASSWORD` replaces the DSN's, and the DSN must then name the user (see
  [Passwords and secrets](/docs/secrets/)). It is sent only to the known password authenticators
  (Cassandra, DSE, ScyllaDB, Instaclustr, Aiven, Amazon Keyspaces, ecAudit). Passwords never
  appear in errors.
- **TLS** is off by default. `tls=true`, or any `tls_*` parameter, turns it on.
- **Plain text.** Without TLS, a password goes to a host that is not this machine (`localhost` or a
  loopback address) only with `allow_plaintext=1`, and tobira then writes a warning to the debug
  log. Amazon Keyspaces (`*.amazonaws.com`, `*.amazonaws.com.cn`) only speaks TLS, so it is
  refused even then.
- The connection's `options:` are refused, and so is any unknown parameter.

| Parameter | Default | Meaning |
| --- | --- | --- |
| `consistency` | `LOCAL_QUORUM` | read and write consistency (`ONE`, `QUORUM`, `LOCAL_ONE`, `ALL` …) |
| `serial_consistency` | `LOCAL_SERIAL` | consistency of edits (`SERIAL` or `LOCAL_SERIAL`) |
| `timeout` | `10s` | per-request timeout |
| `connect_timeout` | `10s` | connect timeout |
| `dc` | none | local datacenter; requests prefer its nodes |
| `tls` | `false` | turn TLS on |
| `tls_ca` | system roots | CA certificate file |
| `tls_server_name` | the host | name to verify the certificate against |
| `tls_insecure` | `false` | skip certificate verification (testing only) |
| `proto` | negotiated | native protocol version, 3 to 5 |
| `discovery` | `true` | `false` talks only to the listed hosts (behind a proxy or NAT) |
| `system_keyspaces` | `false` | also list the `system*` keyspaces |
| `allow_plaintext` | `false` | send a password without TLS to a host that is not this machine |

## Capabilities

| Capability | Support |
| --- | --- |
| Query, keyspaces, tables, describe | yes; keyspaces are the namespaces; describe shows partition and clustering keys |
| Browse | yes; pages follow the server's paging state; sorting is done by tobira, up to 10,000 rows |
| Editing | yes; one row by its whole primary key, as a lightweight transaction |
| Completion | yes; CQL keywords, keyspaces, tables and columns; catalog cached 60 s, refreshed by `:reload` |
| Explain, transactions, watching, native export | no |

Cassandra is a wide-column store: the grid header marks partition columns `[P]` and clustering
columns `[C↓]` or `[C↑]` (see the [TUI guide](/docs/tui/)).

## Queries

A query is one CQL statement.

```sql
SELECT * FROM orders WHERE tenant = 'acme' AND day = '2026-03-04';
SELECT tenant, count(*) FROM orders GROUP BY tenant;
DESCRIBE TABLE orders;
```

- Rows stream in pages of 1,000. Writes and schema changes return an empty result: Cassandra
  reports no row count.
- Parameters (`?` or `:name`) take the column's type from the server, so text fills a `date`,
  `uuid` or `duration` and a list fills a `list<int>`.
- `USE` is refused: choose the keyspace as the namespace instead.
- Running a query in another keyspace than the connection's needs native protocol 5 (Cassandra 4.0
  and later). On ScyllaDB or Cassandra 3, qualify the table names instead.

## Browsing

- **Keyspaces** are listed without `system` and `system_*`, unless `system_keyspaces=true`.
- **Tables and materialized views** are listed with their comments. Row counts are unknown:
  Cassandra only estimates them per node.
- **Describe** lists the partition key columns, then the clustering columns (with their order),
  then the other columns, plus the secondary indexes (SAI included).
- **Filters**: equals, less or greater than, in a list, and contains on a list, set or map column.
  Not equals, starts with, is null and is not null have no CQL form without a search index. A raw
  filter is a CQL `WHERE` body, without `;` or another clause (`LIMIT`, `ORDER BY` and so on).
- Any filter adds `ALLOW FILTERING`, which scans the table unless it restricts the partition key.
  Filter by the partition key to stay fast.
- **Paging** follows the server's paging state, in token and clustering order. A page is 100 rows
  by default, at most 10,000.
- **Sorting** is done by tobira, since CQL sorts only by clustering columns inside one partition:
  it reads the whole filtered table (at most 10,000 rows), sorts it with NULLs last and pages
  through it.

## Editing

Edits are lightweight transactions on the whole primary key, so an edit touches exactly one row or
changes nothing:

- Inserting a row whose key exists fails (a CQL `INSERT` would otherwise overwrite it).
- Updating or deleting a row that is gone fails with "not found".
- Primary key columns cannot be changed: insert a new row and delete the old one.
- Materialized views are read-only.
- Counter tables cannot be edited inline. Change a counter in CQL: `UPDATE … SET c = c + 1`.
- A top-level tuple column cannot be edited inline; write it in CQL. A tuple inside a collection
  or a user-defined type is fine.

There are no transactions: a lightweight transaction covers one row.

## Read-only guard

Cassandra has no read-only session, so `read_only: true` on a connection, or `--read-only` on one
command, is an allow-list checked before anything is sent. Anything it cannot recognise as a read is
refused:

- Exactly one `SELECT`, `DESCRIBE` or `DESC` statement, with an optional trailing `;`. `INSERT`,
  `UPDATE`, `DELETE`, `BATCH`, `TRUNCATE`, every `CREATE`, `ALTER` and `DROP`, `GRANT`, `REVOKE`,
  role statements, `LIST`, `USE` and a second statement after `;` are refused.
- Only built-in functions (`count`, `token`, `writetime`, `ttl`, `toJson`, `floor` and the rest
  of Cassandra 5.0's native functions). A user-defined function or aggregate is refused, whatever
  its spelling. When a user function shares a built-in's name, that name is refused too; write
  `system.abs(x)` to call the built-in. If tobira cannot read the list of user functions, the
  statement is refused.
- Text tobira cannot read to its end the way Cassandra reads it (an unterminated string, name,
  `$$` string or comment, a control character or a stray non-ASCII byte) is refused.

Editing is refused on a read-only connection, and a browse filter is checked as part of the
`SELECT` it becomes. Read-only does not limit cost: `ALLOW FILTERING` is allowed and can scan a
whole table.

## Types

| Cassandra | tobira |
| --- | --- |
| `tinyint`, `smallint`, `int`, `bigint`, `counter` | int |
| `varint`, `decimal` | decimal (exact) |
| `float`, `double` | float |
| `boolean` | bool |
| `text`, `ascii`, `varchar`, `uuid`, `timeuuid`, `inet` | string |
| `duration` | string, `1y2mo3d4h5m6s7ms` |
| `time` | string, `15:04:05.123` |
| `timestamp`, `date` | time (UTC) |
| `blob` | bytes |
| `list`, `set`, `tuple` | list (a set comes back sorted) |
| `map`, user-defined types | object |
| `vector<float, n>` | vector |
| custom types | unknown, with the raw bytes |

- An unset column, an empty non-frozen collection and an empty (zero-length) number, time or uuid
  value read as NULL. Cassandra does not tell NULL and unset apart.
- When you edit, collections, user-defined types and tuples take JSON (`["a","b"]`, `{"k":1}`),
  and numbers, times, dates (`2026-03-04`), UUIDs and durations take text.

## Limits

| Limit | Value |
| --- | --- |
| Browse page | 100 rows by default, at most 10,000 |
| Sorted browsing | at most 10,000 rows after the filter |
| Collection shown in full | 10,000 elements; a bigger one shows as a placeholder naming its size |
| Nesting shown in full | 32 levels |

Rows are held whole, so a page of very wide rows (large blobs) uses a lot of memory. Use a smaller
page size.

## Compatibility

Tested with Cassandra 5.0, one node, with password authentication, user-defined functions and
materialized views. Not yet tested: ScyllaDB (it speaks the same protocol), TLS and multi-node
clusters.

## Troubleshooting

- **"the password would go to … without TLS"**: add `tls=true`. If the network is trusted, add
  `allow_plaintext=1` instead.
- **"… only speaks TLS: add tls=true"**: Amazon Keyspaces needs TLS.
- **Querying another keyspace is not supported**: the server speaks native protocol 4 (ScyllaDB,
  Cassandra 3). Qualify the table names, such as `shop.orders`.
- **A filter is not supported**: CQL has no not-equals, starts-with or is-null relation without a
  search index. Use a raw `WHERE` filter, or add an SAI index.
- **Sorting is refused**: the filtered table has more than 10,000 rows. Filter by the partition key
  first.
- **A read-only query using a function is refused**: the function is user-defined, or a user
  function shares its name. Call the built-in as `system.<name>(…)`.
- **Timeouts behind a proxy or NAT**: the cluster advertises addresses you cannot reach. Add
  `discovery=false`.
