---
layout: ../../../layouts/DocsLayout.astro
title: Vertica
---

# Vertica

The `vertica` driver connects to Vertica (OpenText Analytics Database). It is built into the
`tobira` binary and uses the official Go driver, `github.com/vertica/vertica-sql-go`. You can
query, browse, edit rows, explain queries and run transactions.

## Quick start

```sh
tobira query "vertica://dbadmin@vertica.example.com/analytics?tlsmode=server-strict" "select count(*) from sales" --read-only
```

```yaml
connections:
  - name: warehouse
    dsn: vertica://ada@vertica.example.com/analytics
    password: env:VERTICA_PASSWORD
    read_only: true
```

## Connecting

```text
vertica://user@host[:port][/database][?parameters]
vertica://dbadmin@vertica.example.com/analytics?tlsmode=server-strict
vertica://ada@10.0.0.5:5433/analytics?backup_server_node=10.0.0.6,10.0.0.7&connection_load_balance=1
vertica://vertica.example.com/analytics?auth=oauth&tlsmode=server-strict
```

- The port defaults to 5433. With no database, Vertica uses the user's default.
- A user is required, except with `auth=oauth`.

| Parameter | Effect |
| --- | --- |
| `tlsmode` | `server-strict`, `server`, `prefer` or `none` (see TLS below). |
| `allow_plaintext` | Needed for `server`, `prefer` or `none` to a host other than your machine. |
| `backup_server_node` | Other nodes to try, as `host[:port],...`. |
| `connection_load_balance` | `1` lets the server move you to another node. |
| `workload` | The session's workload name. |
| `client_label` | The label the server shows for the session. |
| `auth=oauth` | The password is an OAuth access token. |

Any other parameter is refused, and so are the connection's `options:`.

### Credentials

Put the password (or OAuth access token) in the connection's `password:` as a
[secret reference](/docs/secrets/), or in `TOBIRA_CONN_<NAME>_PASSWORD`. A DSN that carries a
credential (`user:password@`, `password=`, `oauth_access_token=`) is refused. The secret never
appears in an error or a log.

### TLS and `allow_plaintext=1`

| `tlsmode` | What happens |
| --- | --- |
| `server-strict` | TLS required, and the certificate is checked. The default for any host that is not your machine. |
| `server` | TLS required, but the certificate is not checked. |
| `prefer` | TLS when the server offers it, without checking the certificate. The default on your machine. |
| `none` | No TLS. |

To a host other than your machine, `server`, `prefer` and `none` also need `allow_plaintext=1`.
tobira writes a warning to the debug log for `prefer` and `none`.

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes (namespaces are the database's schemas) |
| Edit rows | yes, by primary key |
| Transactions | yes (`READ COMMITTED` or `SERIALIZABLE`) |
| Explain | yes (`EXPLAIN`; the statement never runs; no `ANALYZE`) |
| Completion | yes (keywords, schemas, tables, columns) |
| Read-only guard | yes |

## Queries

Write Vertica SQL, one statement per query.

```sh
tobira query warehouse "select region, sum(amount) from sales group by region"
tobira query warehouse --explain "select * from sales where customer_id = 7"
```

- Transaction control in a query is refused. Use `:tx begin`.
- The Go driver reads a whole result into memory before showing the first row, so add a `LIMIT`
  to a large query. Browsing always does.
- Cancelling a query ends it on the server; tobira gets control back within 5 seconds.
- `CREATE FUNCTION` for a SQL function is refused, because its body holds a `;`. Use `vsql`.
- In a query result, a `NUMERIC` with more than 15 significant digits loses precision, and the
  result carries a warning. Browsing shows such columns exactly, and so does a cast to `VARCHAR`.

## Browsing

Pages use `LIMIT` and `OFFSET`, ordered by your sort and then the primary key.

## Editing

Edit rows in the grid (`e`, `i`, `d`, then `:apply`). See [Editing rows](/docs/editing/). Each
edit runs in its own transaction.

Vertica enforces primary keys only when they are `ENABLED`, so tobira checks them itself:

- An insert needs the whole key, unless the key is a single generated column (`AUTO_INCREMENT` or `IDENTITY`),
  and a duplicate key is refused. A generated key is shown back after the insert.
- An update or delete must change exactly one row.

## Transactions

`:tx begin` keeps one session and turns autocommit off while the transaction is open.

- DDL commits an open transaction in Vertica, so DDL, `TRUNCATE`, `COPY` and `SET` are refused
  inside one.
- The isolation levels are `READ COMMITTED` and `SERIALIZABLE`.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, or when you
run `:readonly on`. tobira then protects you in three layers:

1. **Allow-list, before anything is sent.** One `SELECT`, `WITH`, `SHOW` or `EXPLAIN` statement,
   with no write, session, transaction, `COPY`, `EXPORT` or `CONNECT` word. Function calls must be
   on a list of built-in functions, because Vertica's meta-functions act from a `SELECT`
   (`SELECT SHUTDOWN()`, `SET_CONFIG_PARAMETER(...)`, `PURGE()`), even in a read-only
   transaction. Text tobira cannot read the way the server does (nested comments, a number glued
   to a word, non-ASCII characters outside strings, quoted names and comments) is refused too.
2. **The server.** Every read-only call runs in `START TRANSACTION READ ONLY` and is rolled back.
   A read-only connection also makes every session's transactions read-only.
3. **On every connection**, writable too: one statement only, and no `COPY ... FROM LOCAL` or
   `COPY ... FROM STDIN`, which would read files or input on your machine.

Refused statements make `tobira query` exit with code 4. Row edits are refused too.

**What it cannot stop.** A user-defined function called from inside a view, or one that overloads
a built-in name. For a database that must never change, connect as a user with only `SELECT`
grants and no `EXECUTE` on user-defined functions.

## Types

| Vertica | tobira type |
| --- | --- |
| `INT` | integer |
| `FLOAT` | float |
| `NUMERIC` | decimal |
| `BOOLEAN` | boolean |
| `VARCHAR`, `CHAR`, `LONG VARCHAR`, `UUID`, `INTERVAL` | text |
| `DATE`, `TIMESTAMP`, `TIMESTAMPTZ` | time |
| `TIME`, `TIMETZ` | text |
| binary types | bytes |
| `ARRAY`, `SET`, `ROW` | their text form |

Values over 1 MiB are cut, with a warning.

## Limits

- One statement per query.
- Results are read into memory in full.
- `NUMERIC` values with more than 15 significant digits lose precision in query results.
- No `ANALYZE` plans and no native export.

## Compatibility

Tested with Vertica 25.4, a single node. Not yet tested: TLS, OAuth, load balancing and backup
nodes, multi-node and Eon-mode databases, and servers older than 25.4.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `tlsmode=... does not verify the server, so a host that is not loopback ... also needs allow_plaintext=1` | Use `tlsmode=server-strict`, or add `allow_plaintext=1`. |
| A TLS handshake error | The server has no verifiable certificate. Fix the certificate, or use `tlsmode=server&allow_plaintext=1`. |
| `credentials must not be in the DSN` | Move the password or token to the connection's `password:`. |
| `malformed DSN: unknown parameter` | Only the parameters in the table above are accepted. |
| `malformed DSN: a user is required` | Add the user, or use `auth=oauth`. |
| `read-only violation` | The statement is outside the allow-list, or calls a function tobira does not know. |
