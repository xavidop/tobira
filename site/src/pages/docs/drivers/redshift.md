---
layout: ../../../layouts/DocsLayout.astro
title: Amazon Redshift
---

# Amazon Redshift

The `redshift` driver connects to Amazon Redshift, which speaks the PostgreSQL wire protocol. It
is built into the `tobira` binary and shares the [PostgreSQL driver](/docs/drivers/postgres/)'s
code. It is for reading: you can query, browse, explain and use completion. Editing,
transactions, `COPY` export and `LISTEN` are not available, because Redshift lacks what they need.

## Quick start

```sh
tobira query "redshift://analyst@cluster.abc123.eu-west-1.redshift.amazonaws.com:5439/dev?sslmode=verify-full" \
  "select count(*) from sales" --read-only
```

```yaml
connections:
  - name: warehouse
    dsn: redshift://analyst@cluster.abc123.eu-west-1.redshift.amazonaws.com:5439/dev?sslmode=verify-full
    password: env:REDSHIFT_PASSWORD
    read_only: true
```

## Connecting

| DSN | Result |
| --- | --- |
| `redshift://user@cluster:5439/dev?sslmode=verify-full` | Redshift, through the `redshift` driver |
| `postgres://user@cluster:5439/dev?flavor=redshift` | the same, through the `postgres` driver |
| `postgres://user@cluster:5439/dev` | tobira asks the server when it connects and detects Redshift |

Use `redshift://` or `flavor=redshift` for a read-only connection. With a plain `postgres://`
DSN, tobira only learns it is Redshift after connecting, and by then it has already asked for a
read-only session setting that Redshift does not document.

The DSN, password, TLS (`sslmode`) and `options:` rules are those of the
[PostgreSQL driver](/docs/drivers/postgres/#connecting).

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Explain | yes (Redshift's text plan, shown as a tree; no `EXPLAIN ANALYZE`) |
| Completion | yes |
| Edit rows, transactions | no (Redshift has no savepoints and no `RETURNING`) |
| Native csv export | no (no `COPY ... TO STDOUT`); `--output` uses tobira's own csv |
| Watch | no (no `LISTEN`/`NOTIFY`) |
| Vector search | no |
| Read-only guard | yes |

## Browsing

- Row counts and sizes come from `SVV_TABLE_INFO`, which lists only tables that hold data. Other
  tables show an unknown count.
- Redshift has no indexes. The description shows the primary key and foreign keys (Redshift's
  informational constraints), plus the distribution key as `distkey` and the sort key as
  `compound sortkey` or `interleaved sortkey`.
- Tables in schemas outside your `search_path` are described too.

Queries and paging work as described in
[PostgreSQL queries, browsing and editing](/docs/drivers/postgres/queries/).

## Read-only guard

Each read-only call runs in `BEGIN READ ONLY ... ROLLBACK`, one statement at a time, with the
refusals of the [PostgreSQL read-only guard](/docs/drivers/postgres/read-only/) (`COPY`, `DO`,
server-file functions).

Redshift's `UNLOAD` writes query results to S3 without writing to the database, so a read-only
transaction does not stop it. On a read-only connection tobira therefore only runs statements
that start with `SELECT`, `WITH`, `VALUES`, `TABLE`, `SHOW` or `(`, or `EXPLAIN [VERBOSE]` of one
of those. `UNLOAD`, `COPY`, `CALL`, `SET`, `SELECT ... INTO` and every write are refused before
they reach the server.

**What it cannot stop.** Redshift has no `DISCARD`, so tobira resets a reused connection with
`RESET ALL`. That restores settings but not temp tables or `SET SESSION AUTHORIZATION` made by an
earlier query on a writable connection. Use a user with `SELECT` only as the real boundary.

## Compatibility

Tested by running the PostgreSQL 17 suite in Redshift mode, with Redshift's system views
(`SVV_TABLE_INFO`, `PG_TABLE_DEF`) added. Not yet tested against a real Redshift cluster. If
something fails there, please open an issue.
