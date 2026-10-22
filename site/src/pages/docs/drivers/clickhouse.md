---
layout: ../../../layouts/DocsLayout.astro
title: ClickHouse
---

# ClickHouse

The `clickhouse` driver connects to ClickHouse. It is built into the `tobira` binary and uses the
official Go client, `github.com/ClickHouse/clickhouse-go`. It speaks the native protocol by
default, or HTTP when you ask for it. You can query, browse, explain queries and export to CSV.
Editing rows is not available (see [Editing](#editing)).

## Quick start

```sh
tobira query "clickhouse://reader@ch.internal:9440/analytics?secure=true" "select count() from events" --read-only
```

```yaml
connections:
  - name: analytics
    dsn: clickhouse://reader@ch.internal:9440/analytics?secure=true
    password: env:CH_PASSWORD
    read_only: true
```

## Connecting

```text
clickhouse://user@localhost:9000/default
clickhouse://user@ch.internal/analytics?secure=true
clickhouse://user@ch.example.com:8443/analytics?protocol=http&secure=true
clickhouse://user@ch1,ch2:9001/analytics
```

- The port defaults to 9000 (native), or 9440 with `secure=true`.
- `protocol=http` switches to the HTTP interface, on 8123 by default (8443 with `secure=true`).
  ClickHouse Cloud and many proxies only expose HTTPS.
- Several hosts, separated by commas, are tried in order. Put IPv6 hosts in brackets.
- The Go client's parameters work (`secure`, `skip_verify`, `dial_timeout`, `compress` and so
  on). Any other key is sent to the server as a setting with every query.
- The connection's `options:` are refused. Put settings in the DSN.

### Password

Keep the password out of the DSN. Use the connection's `password:` setting with a
[secret reference](/docs/secrets/), or `TOBIRA_CONN_<NAME>_PASSWORD`. Either one replaces the
DSN's password. Errors never show it.

### TLS and `allow_plaintext=1`

| Setting | Effect |
| --- | --- |
| `secure=true` | TLS, with the server certificate checked. |
| `secure=true&skip_verify=true` | TLS without checking the certificate. Use it only for a test server. |
| no `secure` | No TLS. |

- Over HTTP without `secure=true`, a host other than your machine also needs
  `allow_plaintext=1`. Plain HTTP to ClickHouse Cloud (`*.clickhouse.cloud`) is always refused.
- Over the native protocol without `secure=true`, a connection with a password to a host other than
  your machine also needs `allow_plaintext=1`, since the password would travel in clear. It is
  always refused for ClickHouse Cloud. A connection without a password is not checked.
- tobira never follows an HTTP redirect, so your credentials cannot be forwarded to another host.

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes (without `system`, `INFORMATION_SCHEMA` and `information_schema`) |
| Explain | yes (the plan with index usage, as text; no `ANALYZE`) |
| Completion | yes (keywords, databases, tables, columns) |
| Native csv export | yes (`CSVWithNames`) |
| Edit rows, transactions | no |
| Read-only guard | yes |

## Queries

Write ClickHouse SQL, one statement per query.

```sh
tobira query analytics "select event, count() from events group by event order by 2 desc limit 10"
tobira query analytics --explain "select * from events where user_id = 7"
```

- **Writes.** On a writable connection, `INSERT`, `ALTER`, DDL and other statements that return no
  rows run as they are and report no row count.
- **Databases.** A query run in another database than the connection's opens a separate small
  connection pool for that database, kept until the connection closes.
- **Cancelling** a query, or closing its result early, stops it on the server.

## Browse

Browsing runs one `SELECT *` through the same path as a query, so on a read-only connection the
guard checks the generated text too. Filter values are sent as parameters.

- **Filters**: equals, not equals, less or greater than, contains and starts with (compared as
  text), in a list, is null and is not null. A raw `WHERE` filter is native SQL that must keep its
  parentheses balanced and hold no `;`.
- **Paging** uses `LIMIT` and `OFFSET`, ordered by your sort and then by the table's sorting key
  (by every sortable column when the table has none). NULLs sort last ascending and first
  descending. A sorting key need not be unique, so rows tied on it can move between pages if the
  table changes while you browse.

## Describe

The primary key shown is the table's sorting key, when every part of it is a plain column; a key
with an expression, such as `toDate(ts)`, shows no key columns. The indexes are the primary index
and the data-skipping indexes. Default, materialized and alias columns are reported, with
materialized and alias columns marked as generated. The definition is the table's
`CREATE TABLE` statement.

## Explain

`--explain` and `:explain` show `EXPLAIN indexes = 1`: the plan, and the parts and granules each
index selects. ClickHouse never runs a query to explain it and has no `EXPLAIN ANALYZE`.

## Export

Exporting to CSV uses ClickHouse's `CSVWithNames`: a header row of column names, then the rows,
with NULL written as `\N`. Over HTTP the server formats the file and tobira streams it; over the
native protocol tobira writes the same layout. Leave any `FORMAT` clause out of the query.

## Editing

The grid is read-only for ClickHouse. Run writes as queries on a writable connection.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, or when you
run `:readonly on`. tobira then protects you in two layers:

1. **An allow-list, before anything is sent.** Only `SELECT`, `WITH`, `SHOW`, `DESCRIBE`,
   `EXPLAIN` and `EXISTS` are accepted. Text that cannot be read to its end, several statements,
   `INTO OUTFILE` and `KILL` are refused.
2. **The server's own read-only mode.** Every read-only query is sent with the setting
   `readonly=1`, whatever the DSN says. ClickHouse then refuses every write, every schema change,
   `SYSTEM` and `OPTIMIZE`, any change of settings (including a `SETTINGS` clause), and the table
   functions that reach outside the server, such as `url`, `s3`, `file` and `remote`.

If your ClickHouse user is already read-only (its profile sets `readonly` to 1 or 2, as is common for
BI accounts), tobira detects it when connecting and relies on the server's own read-only mode for
writes and schema changes, since ClickHouse refuses to let such a user change it. Under `readonly=2`
ClickHouse does not block table functions, so for these users tobira checks them itself. Before
each read-only query, export or browse, it asks ClickHouse to parse the text (`EXPLAIN AST`, one
extra round trip) and reads the tree:

- Only `numbers`, `numbers_mt`, `zeros`, `zeros_mt`, `generateRandom`, `values` and `null` may be
  used as table functions, wherever a table can go (`FROM`, `JOIN`, `DESCRIBE`, subqueries, CTEs) or
  on the right of `IN`. Every other table function, such as `url`, `s3`, `remote`, `merge` or any
  `*Cluster` one, gives a read-only error.
- Functions that reach outside the server, such as `file()`, are refused anywhere.
- When ClickHouse cannot parse the text, or tobira cannot read the tree, the query is refused rather
  than sent. Quoted names holding control characters or backslashes are refused too.

Queries with parameters are refused on a read-only connection when a placeholder sits inside a
`$$...$$` string or a value contains `$$`.

**What it cannot stop.** `system.*` tables can still be read. Tables and dictionaries already on
the server that read from outside (`URL` or `S3` engines, `dictGet` on an external source) are
not table functions, so tobira does not refuse them; your server's grants decide who can read
them.

ClickHouse keeps no session between queries, so nothing a query does carries over to the next one.
Browsing, explaining, completion and the catalog on a read-only connection run the same way.
Export always runs read-only.

## Types

| ClickHouse | Shown as |
| --- | --- |
| Int types up to 64 bits, UInt types up to 32 bits | integer |
| UInt64, Int128, Int256, UInt128, UInt256, Decimal | decimal, exact |
| Float32, Float64 | float |
| Bool | boolean |
| String, FixedString, Enum, UUID, IPv4, IPv6 | text |
| Date, DateTime, DateTime64 | time |
| Array, Map, Tuple, Nested, JSON, Variant, Dynamic | JSON |
| Nullable and LowCardinality columns | as the inner type |

## Limits

- No row edits and no transactions.
- No `EXPLAIN ANALYZE`.
- A sorting key need not be unique, so rows tied on it can move between pages while the table
  changes.

## Compatibility

Tested with ClickHouse 25.8, over the native protocol and over HTTP.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `plain HTTP to <host> (not loopback) needs allow_plaintext=1, or secure=true for TLS` | Add `secure=true`, or `allow_plaintext=1` if you accept sending the password in clear. |
| `a password over the plain native protocol to <host> (not loopback) needs allow_plaintext=1, or secure=true for TLS` | Add `secure=true` (port 9440), or `allow_plaintext=1` if you accept sending the password in clear. |
| `plain HTTP is refused for ClickHouse Cloud`, `a password over the plain native protocol is refused for ClickHouse Cloud` | Add `secure=true`. |
| `unsupported protocol` | Use `protocol=native` or `protocol=http`. |
| `unsupported option ...: the clickhouse driver takes its settings from DSN parameters` | Move the setting from `options:` into the DSN. |
| `read-only violation` | The statement is outside the allow-list, uses a table function tobira does not allow, or ClickHouse refused it under `readonly`. |
