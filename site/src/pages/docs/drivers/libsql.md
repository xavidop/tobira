---
layout: ../../../layouts/DocsLayout.astro
title: libSQL / Turso
---

# libSQL / Turso

The `libsql` driver connects to [libSQL](https://github.com/tursodatabase/libsql) servers:
`sqld` (libsql-server) and [Turso](https://turso.tech). It is built into the `tobira` binary and
talks to the server over HTTP. The SQL is SQLite's. You can query, browse, edit rows, explain
queries and run transactions. For local SQLite files, use the [SQLite driver](/docs/drivers/sqlite/).

## Quick start

```sh
tobira query "libsql://my-db-my-org.turso.io" "select * from users limit 5" --read-only
```

```yaml
connections:
  - name: edge
    driver: libsql
    dsn: libsql://my-db-my-org.turso.io
    password: keyring:turso-edge
```

## Connecting

| Form | Example |
| --- | --- |
| `libsql://host[:port]` | `libsql://my-db-my-org.turso.io` (HTTPS) |
| `libsql://host:port?tls=0` | `libsql://localhost:8080?tls=0` (plain HTTP) |
| `https://host[:port][/path]` | `https://sqld.internal:8443` |
| `http://host[:port][/path]` | `http://127.0.0.1:8080` |
| `wss://host`, `ws://host` | the same as `https://` and `http://` |

- Only `libsql://` selects the driver by itself. For `http(s)://` and `ws(s)://` URLs, set
  `driver: libsql` on the connection.
- The DSN accepts only `tls` (on `libsql://` URLs) and `allow_plaintext`. The connection's
  `options:` are refused.
- tobira contacts the server when it opens the connection, so a wrong address or token fails at
  once.

### Auth token

Put the Turso database token (or a JWT for sqld's `--auth-jwt-key`) in the connection's
`password:`, as a [secret reference](/docs/secrets/), or in `TOBIRA_CONN_<NAME>_PASSWORD`.
`libsql://:TOKEN@host` also works and is masked like any password.

A token in a query parameter (`authToken`, `auth_token`, `jwt`, `token`) is refused, because
query parameters are not masked. The token is sent as `Authorization: Bearer ...`, never appears
in an error, and is never sent to another host: tobira does not follow redirects.

For a Turso connection that only reads, create a read-only token
(`turso db tokens create --read-only`). The server then refuses every write.

### TLS and `allow_plaintext=1`

`libsql://` and `https://` always use TLS. Plain HTTP (`http://`, `ws://` or `libsql://...?tls=0`)
is allowed without more to a server on your machine (`localhost` or a loopback address). To
another machine it also needs `allow_plaintext=1`:

```text
libsql://sqld.lan:8080?tls=0&allow_plaintext=1
```

Plain HTTP to Turso (`*.turso.io`) is always refused.

With plain HTTP the token crosses the network in clear, readable by anyone on the path, including
an HTTP proxy. tobira writes a warning to the debug log when it sends a token that way.

The driver honours `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`. HTTPS goes through a proxy as a
tunnel; plain HTTP, token included, does not. Set `NO_PROXY` for the database host if that
matters.

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (`EXPLAIN QUERY PLAN`; no `ANALYZE`) |
| Completion | yes |
| Read-only guard | yes |

## Queries and browsing

Write SQLite SQL, one statement per query.

```sh
tobira query edge "select id, email from users order by id desc limit 20"
tobira query edge --explain "select * from users where email = 'a@example.com'"
```

The server sends a statement's whole result at once; `--limit` only trims what tobira keeps. Add a
`LIMIT` to queries over large tables. Browsing pages with `LIMIT` and `OFFSET`, ordered by your
sort and then the primary key. Browsing, describing tables and editing rows work as in the
[SQLite driver](/docs/drivers/sqlite/#browsing).

## Transactions

`:tx begin` starts a transaction with `BEGIN IMMEDIATE`. tobira keeps it alive while it is open,
because the server closes idle sessions after about 10 seconds.

- `COMMIT`, `END`, `BEGIN` and `ROLLBACK` are refused inside the transaction. Use `:tx commit` and
  `:tx rollback`.
- tobira checks that every statement stayed inside the transaction.
- A read-only transaction uses the read-only guard, and its commit rolls back.
- If a request inside a writable transaction fails on the network (cancelled, timed out,
  dropped), the transaction is rolled back. The server may keep its write lock for about 10
  seconds, so other writers wait or fail with a conflict until then.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, or when you
run `:readonly on`. sqld does not allow `PRAGMA query_only` or `ATTACH`, so tobira protects you
in three ways:

1. **Allow-list, before anything is sent.** The same rules as the
   [SQLite driver](/docs/drivers/sqlite/#read-only-guard): one `SELECT`, `WITH`, `VALUES`,
   `EXPLAIN` or reading pragma. A read that holds `INSERT`, `UPDATE`, `DELETE` or `REPLACE INTO`
   (a writable `WITH`) is refused, and so is `load_extension`.
2. **Rolled back on the server.** The statement runs between `BEGIN` and `ROLLBACK`, so anything
   that got past the first check is undone.
3. **A read-only token**, if you use one. The server refuses every write, and browsing still
   works.

Refused statements make `tobira query` exit with code 4. Row edits are refused too.

**What it cannot stop.** Other clients with a writable token. For a hard guarantee, use a
read-only token.

## Types

The server reports each column's declared type, so values convert as in the
[SQLite driver](/docs/drivers/sqlite/#types): `DECIMAL(p,s)` is a decimal, `BOOLEAN` a boolean,
`DATETIME` and `TIMESTAMP` a time, `JSON` JSON and `BLOB` bytes. Expression columns take the type
of the first row's value.

## Limits

- One statement per query.
- A response over 64 MiB, or with more than 4096 columns, fails with `size limit exceeded`.
- Cancelling a query returns at once, but sqld may still finish the statement.
- Statements sqld does not support (`VACUUM`, `ATTACH`) fail as unsupported.

## Compatibility

Tested with sqld 0.24 with JWT authentication. Not yet tested against Turso itself, which serves
the same protocol.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `plain HTTP to <host> (not loopback) needs allow_plaintext=1` | Use `libsql://` or `https://`, or add `allow_plaintext=1` if you accept sending the token in clear. |
| `plain HTTP is refused for Turso` | Use `libsql://` or `https://`. |
| `put the auth token in the password field ...` | Move the token from the URL parameter to the connection's `password:`. |
| `libsql needs a server URL` | The DSN is a file path. Use the SQLite driver for local files. |
| `tls applies only to libsql:// URLs` | Use `http://` or `https://` instead of `tls=` on those URLs. |
| `permission denied` | The token is read-only, or lacks access to the database. |
| `size limit exceeded` | Add a `LIMIT` to the query. |
