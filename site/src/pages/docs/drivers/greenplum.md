---
layout: ../../../layouts/DocsLayout.astro
title: Greenplum
---

# Greenplum

The `greenplum` driver connects to Greenplum, which is built on PostgreSQL. It is built into the
`tobira` binary, shares the [PostgreSQL driver](/docs/drivers/postgres/)'s code and has all of
its capabilities.

## Quick start

```sh
tobira query "greenplum://gpadmin@coordinator.internal:5432/warehouse" "select count(*) from events" --read-only
```

```yaml
connections:
  - name: gp
    dsn: greenplum://gpadmin@coordinator.internal:5432/warehouse?sslmode=verify-full
    password: env:GP_PASSWORD
```

## Connecting

| DSN | Result |
| --- | --- |
| `greenplum://user@coordinator:5432/db` | Greenplum, through the `greenplum` driver |
| `postgres://user@coordinator:5432/db?flavor=greenplum` | the same, through the `postgres` driver |
| `postgres://user@coordinator:5432/db` | tobira asks the server when it connects and detects Greenplum |

The DSN, password, TLS (`sslmode`) and `options:` rules are those of the
[PostgreSQL driver](/docs/drivers/postgres/#connecting).

## Capabilities

Everything the [PostgreSQL driver](/docs/drivers/postgres/#capabilities) offers: browsing,
`EXPLAIN (FORMAT JSON)` with `ANALYZE` rolled back, editing, transactions, completion, `COPY`
export, `LISTEN`, and vector search when the database has the pgvector extension. The
[read-only guard](/docs/drivers/postgres/read-only/) is the PostgreSQL driver's.

## Browsing

Greenplum 6 is based on PostgreSQL 9.4 and Greenplum 7 on PostgreSQL 12, so tobira reads only
what both have:

- generated and identity columns are not marked as such (serial columns are);
- on Greenplum 7, the `INCLUDE` columns of an index show as index keys;
- the `gp_toolkit`, `pg_aoseg`, `pg_bitmapindex` and `pg_ext_aux` schemas are hidden.

The description shows a hash-distributed table's distribution key as `distributed by`. Randomly
distributed and replicated tables have none. On Greenplum 5 the distribution key is not shown.

## Compatibility

Tested by running the PostgreSQL 17 suite in Greenplum mode, with Greenplum's
`gp_distribution_policy` catalog added. Not yet tested against a real Greenplum 6 or 7 cluster. If
something fails there, please open an issue.
