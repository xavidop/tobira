---
layout: ../../../../layouts/DocsLayout.astro
title: PostgreSQL
---

# PostgreSQL

The `postgres` driver connects to PostgreSQL. It is built into the `tobira` binary and uses
`github.com/jackc/pgx/v5`. You can query, browse, edit rows, explain queries, run transactions,
export with `COPY`, follow `LISTEN` channels and, with the pgvector extension, run similarity
searches. The same driver also reaches Amazon Redshift, CockroachDB and Greenplum (see
[PostgreSQL-compatible databases](#postgresql-compatible-databases)).

This page covers connecting and the basics. The rest of the reference is split into:

- [Queries, browsing and editing](/docs/drivers/postgres/queries/): running SQL, the schema
  browser, explain, inline edits and transactions.
- [Read-only guard](/docs/drivers/postgres/read-only/): what a read-only connection refuses, and
  what it cannot stop.
- [LISTEN and COPY export](/docs/drivers/postgres/listen-and-export/): live tails of a channel and
  native csv export.
- [pgvector](/docs/drivers/pgvector/): similarity search.

## Quick start

```sh
tobira query "postgres://reader@db.internal/app?sslmode=verify-full" "select now()" --read-only
tobira connect "postgres://app@localhost:5432/app?sslmode=disable"
```

A connection in the config file, with the password kept out of the DSN:

```yaml
connections:
  - name: app
    dsn: postgres://app@db.internal:5432/app?sslmode=verify-full
    password: env:APP_PG_PASSWORD
    read_only: true
```

## Connecting

The driver accepts URL DSNs and libpq `key=value` strings:

```text
postgres://user@localhost:5432/app?sslmode=require
postgresql://user@db.internal/app?sslmode=verify-full&sslrootcert=/etc/ssl/ca.pem
host=localhost port=5432 dbname=app user=reader sslmode=disable
```

On the command line, `tobira query` and `tobira connect` treat an argument as a DSN only when it
contains `://`. Put a `key=value` DSN in the config file.

tobira connects once when it opens the connection, so a wrong host, database or password fails
at once (exit code 2 from `tobira query`).

### Password

Keep the password out of the DSN. Use the connection's `password:` setting with a
[secret reference](/docs/secrets/), or the `TOBIRA_CONN_<NAME>_PASSWORD` environment variable.
Either one overrides a password in the DSN. tobira never prints the password in an error.

### TLS

TLS is set with the libpq parameters `sslmode`, `sslrootcert`, `sslcert` and `sslkey`.

| `sslmode` | What happens |
| --- | --- |
| `disable` | No TLS. |
| `allow`, `prefer` | TLS if the server offers it, otherwise plain text. `prefer` is the default. |
| `require` | TLS, without checking the certificate. |
| `verify-ca` | TLS, and the certificate must be signed by a trusted CA. |
| `verify-full` | TLS, the certificate must be trusted and must match the host name. |

Use `verify-full` for any server that is not on your machine. The `allow_plaintext=1` rule of the
HTTP drivers does not apply here: `sslmode` alone decides.

### Options

The connection's `options:` set session parameters. Only these three are accepted; any other key
fails with `unsupported option`.

| Option | Effect |
| --- | --- |
| `application_name` | The name the server shows for the session. Defaults to `tobira`. |
| `search_path` | The schemas searched for unqualified names. |
| `statement_timeout` | Cancel a statement that runs longer than this (for example `30s`). |

```yaml
connections:
  - name: reports
    dsn: postgres://reader@db.internal/app?sslmode=verify-full
    options:
      application_name: tobira-reports
      search_path: reporting,public
      statement_timeout: 60s
```

Each query starts from a clean session, so a `SET search_path` in one query does not carry over to
the next. Use the `search_path` option instead.

### DSN parameters tobira checks

| Parameter | Rule |
| --- | --- |
| `flavor` | Forces a flavor: `postgres`, `redshift`, `cockroachdb` (or `cockroach`, `crdb`), `greenplum` (or `gpdb`). tobira removes it before connecting. |
| `default_query_exec_mode` | Only `cache_describe`, `describe_exec` and `cache_statement` are accepted. `simple_protocol` and `exec` are refused, because tobira needs them off to keep one statement per query. |
| `options` | On a read-only connection, it must not set `default_transaction_read_only` or similar settings. |

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (`EXPLAIN (FORMAT JSON)`; `ANALYZE` is always rolled back) |
| Completion | yes (keywords, schemas, tables, columns) |
| Native csv export | yes (`COPY ... TO STDOUT`) |
| Watch | yes (`LISTEN` on a channel) |
| Vector search | yes, when the database has the [pgvector](/docs/drivers/pgvector/) extension |
| Read-only guard | yes |

## PostgreSQL-compatible databases

Three more drivers use this same code, each with only the capabilities its database has:

| Database | Driver | DSN scheme | Page |
| --- | --- | --- | --- |
| Amazon Redshift | `redshift` | `redshift://` | [Amazon Redshift](/docs/drivers/redshift/) |
| CockroachDB | `cockroachdb` | `cockroachdb://`, `cockroach://` | [CockroachDB](/docs/drivers/cockroachdb/) |
| Greenplum | `greenplum` | `greenplum://` | [Greenplum](/docs/drivers/greenplum/) |

With a plain `postgres://` DSN, tobira asks the server what it is when it connects and adapts.
To skip the check, use the database's own scheme or add `flavor=`. A scheme and a `flavor=` that
disagree are refused.

## Types

| PostgreSQL | tobira type |
| --- | --- |
| int2, int4, int8 | integer |
| numeric | decimal, keeping its scale (`12.50`) |
| float4, float8 | float |
| bool | boolean |
| text, varchar, bpchar, name | text |
| uuid | text (canonical form) |
| bytea | bytes |
| timestamptz, timestamp, date | time in UTC (a `timestamp` without zone is read as UTC) |
| json, jsonb | JSON |
| arrays | list, nested per dimension |
| vector (pgvector) | vector |
| interval, enums, ranges, geometric types, infinite dates, others | shown as their text form, with the type name |

NULL is always NULL. When you edit a cell, decimals are sent as text so the server casts them,
and a vector is sent as `[1,2,3]`.

## Limits

- One statement per query. Text with two statements is refused by the server.
- Intervals, enums, ranges and geometric types are shown as text.
- The table description has no `CREATE` statement.
- Browsing a table whose primary key is not totally ordered (a float key holding NaN) needs an
  explicit sort to page reliably.

## Compatibility

Tested with PostgreSQL 17, with and without pgvector 0.8.7 installed.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `authentication failed` | Check the user and the password reference. Run `tobira doctor` to see whether the secret resolves. |
| `connection failed` | Check the host, port and `sslmode`. A server without TLS needs `sslmode=disable` or `prefer`. |
| `invalid DSN` | Fix the DSN syntax. The message never shows your password. |
| `unsupported option` | Remove the key from `options:`. Only `application_name`, `search_path` and `statement_timeout` are accepted. |
| `default_query_exec_mode=... is not supported` | Remove the parameter, or use `cache_describe`. |
| `read-only violation` | The connection is read-only. See [Read-only guard](/docs/drivers/postgres/read-only/). |
| `the transaction may be aborted; :tx rollback to continue` | PostgreSQL refuses every statement after an error in a transaction. Run `:tx rollback`. |
| `... is not available on redshift` (or another flavor) | That database does not support the feature. See its page. |
