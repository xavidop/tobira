---
layout: ../../../layouts/DocsLayout.astro
title: CockroachDB
---

# CockroachDB

The `cockroachdb` driver connects to CockroachDB, which speaks the PostgreSQL wire protocol. It is
built into the `tobira` binary and shares the [PostgreSQL driver](/docs/drivers/postgres/)'s code,
so most of that reference applies. You can query, browse, edit rows, explain queries, run
transactions and export with `COPY`.

## Quick start

```sh
tobira query "cockroachdb://reader@crdb.internal:26257/app?sslmode=verify-full" "show tables" --read-only
```

```yaml
connections:
  - name: crdb
    dsn: cockroachdb://reader@crdb.internal:26257/app?sslmode=verify-full
    password: env:CRDB_PASSWORD
```

## Connecting

| DSN | Result |
| --- | --- |
| `cockroachdb://user@host:26257/db?sslmode=verify-full` | CockroachDB, through the `cockroachdb` driver |
| `cockroach://user@host:26257/db` | the same |
| `postgres://user@host:26257/db?flavor=cockroachdb` | the same, through the `postgres` driver |
| `postgres://user@host:26257/db` | tobira asks the server when it connects and detects CockroachDB |

The DSN, password, TLS (`sslmode`) and `options:` rules are those of the
[PostgreSQL driver](/docs/drivers/postgres/#connecting). A connection detected through
`postgres://` shows `postgres` in its errors but offers only CockroachDB's capabilities.

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes (the `crdb_internal` and `pg_extension` schemas are hidden) |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (CockroachDB's text plan, shown as a tree; `ANALYZE` is always rolled back) |
| Completion | yes |
| Native csv export | yes (`COPY ... TO STDOUT`) |
| Watch | no (CockroachDB has no `LISTEN`/`NOTIFY`) |
| Vector search | no |
| Read-only guard | yes |

Queries, browsing, editing and transactions work as described in
[PostgreSQL queries, browsing and editing](/docs/drivers/postgres/queries/), with these
differences:

- A table's size is not shown, and its row estimate is unknown until CockroachDB has statistics
  for it.
- `EXPLAIN` has no JSON format. tobira reads the tree from CockroachDB's text plan, with the
  table and the estimated row count of each node.

## Read-only guard

A read-only connection gets every protection of the
[PostgreSQL read-only guard](/docs/drivers/postgres/read-only/). CockroachDB honours
`default_transaction_read_only=on`.

Some CockroachDB statements reach outside the database without writing to it, so a read-only
transaction does not stop them: `EXPORT`, `BACKUP` and `CREATE CHANGEFEED` write to external
storage, and `crdb_internal` functions such as `force_error` or `reset_sql_stats` act on the
cluster. So on a read-only connection tobira only runs:

- statements that start with `SELECT`, `WITH`, `VALUES`, `TABLE`, `SHOW` or `(`;
- `EXPLAIN [ANALYZE] [VERBOSE]` of one of those, with only the options `VERBOSE`, `TYPES`, `OPT`,
  `VEC`, `DISTSQL`, `GIST` and `REDACT`.

It also refuses, wherever they appear:

- `SHOW BACKUP`, `SHOW BACKUPS` and any `SHOW` with a quoted string (a storage location),
  including `[SHOW ...]` inside a subquery;
- `SELECT ... INTO`;
- any call to a `crdb_internal` function, in any quoting. Reading `crdb_internal` tables is left
  to CockroachDB's own permissions.

Everything else, including `SET`, `CANCEL`, `EXPORT`, `BACKUP` and every write, is refused before
it reaches the server. The same checks apply to the native csv export.

**What it cannot stop.** Anything a function can do on the server with your role's privileges.
Use a role with `SELECT` only as the real boundary.

## Compatibility

Tested with CockroachDB 26.2, a single node in secure mode with password authentication.
