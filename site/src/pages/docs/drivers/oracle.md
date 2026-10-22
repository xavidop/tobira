---
layout: ../../../layouts/DocsLayout.astro
title: Oracle
---

# Oracle

The `oracle` driver connects to Oracle Database. It is built into the `tobira` binary and uses the
pure-Go `github.com/sijms/go-ora/v2`, so you need no Oracle Instant Client. You can query, browse,
edit rows, see estimated plans and run transactions.

## Quick start

```sh
tobira query "oracle://reader@db.internal:1521/ORCLPDB1" "select sysdate from dual" --read-only
```

```yaml
connections:
  - name: ledger
    dsn: oracle://app@db.internal:1521/ORCLPDB1?SSL=true
    password: env:LEDGER_ORACLE_PASSWORD
```

## Connecting

```text
oracle://user@localhost:1521/FREEPDB1
oracle://user@db.internal/ORCLPDB1?SSL=true
oracle://user@db.internal/?connStr=(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=db)(PORT=1521))(CONNECT_DATA=(SERVICE_NAME=app)))
```

- The path is the **service name**. The port defaults to 1521. Put IPv6 hosts in brackets.
- Escape special characters in a password in the URL (`%40` for `@`), or better, keep it out of
  the DSN.
- go-ora's options work, for example `SSL`, `SSL VERIFY`, `WALLET`, `PREFETCH_ROWS`, `AUTH TYPE`
  and `connStr` for a full connect descriptor.
- The connection's `options:` are refused. Put settings in the DSN.
- tobira connects when it opens the connection, so a wrong host or password fails at once.

### Password

Use the connection's `password:` setting with a [secret reference](/docs/secrets/), or
`TOBIRA_CONN_<NAME>_PASSWORD`. Either one replaces the DSN's password. Errors never show it.

### TLS

| Option | Effect |
| --- | --- |
| not set | No TLS. |
| `SSL=true` | TLS, with the server certificate checked. |
| `SSL VERIFY=false` | Together with `SSL=true`: TLS without checking the certificate. Use it only for a test server. |
| `WALLET=/path/to/wallet` | Use an Oracle wallet for certificates. |

Use `SSL=true` for any server that is not on your machine. The `allow_plaintext=1` rule of the
HTTP drivers does not apply here.

### Refused options

| Option | Why |
| --- | --- |
| `TRACE FILE`, `TRACE DIR` | go-ora would write the wire protocol, credentials included, to disk. |
| `LOB FETCH=STREAM` (or `POST`) | tobira reads LOBs together with their row. |
| A `PASSWORD` inside `connStr` | go-ora would send it to the listener in plain text. Put the password in a secret reference. |

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (`EXPLAIN PLAN`; estimated only, the statement never runs) |
| Completion | yes (keywords, schemas, tables, columns) |
| Read-only guard | yes, on Oracle Database 12c to 23ai |

`--output` and `:export` use tobira's own writers.

## Queries

Write Oracle SQL. Each query is one statement, optionally ended by one `;`. Binds are `:1`, `:2`
(or `:name`, bound in order).

```sh
tobira query ledger "select * from invoices fetch first 10 rows only"
tobira query ledger --explain "select * from invoices where customer_id = 7"
```

- Outside a transaction, each statement is committed when it ends.
- A statement that is not a plain query (`ALTER SESSION`, a PL/SQL block) runs on a session that
  is closed afterwards, so its settings never reach later queries.
- Cancelling a query (`ctrl+c`) stops it on the server, and the session stays usable.

## Browsing

The sidebar lists schemas that Oracle does not maintain, plus your own schema (the default).
Tables, views and materialized views show their row estimate (`NUM_ROWS`) and comments.
Describing a table shows columns (virtual columns as generated, identity columns as
auto-increment), defaults, comments, the primary key, foreign keys and indexes. Sizes are not
shown, because they need DBA views.

- Names match exactly as Oracle stores them; if nothing matches, the upper-case name is tried, so
  `orders` finds `ORDERS`.
- `~` (contains) and `^=` (starts with) compare `TO_CHAR(column)` with `LIKE`.
- A `!` native filter must keep its parentheses balanced and hold no `;`.
- Tables with a primary key page by key. A sort pages with `OFFSET ... FETCH`. A table without a
  primary key is ordered by `ROWID`, a view by every column.
- NULLs sort last ascending and first descending.

## Explain

`--explain` and `:explain` run `EXPLAIN PLAN`, then show `DBMS_XPLAN.DISPLAY` as text and the plan
rows as a tree (operation, object, cost and cardinality). The plan rows are rolled back.
`EXPLAIN PLAN` never runs the statement, so `ANALYZE` is not supported and binds are planned
without values.

You can explain one `SELECT`, `WITH`, `INSERT`, `UPDATE`, `DELETE` or `MERGE`, without PL/SQL,
database links or side-effect packages. On a read-only connection a `DELETE` is planned and never
runs. Inside a writable transaction the plan does not disturb your changes. A read-only
transaction cannot explain.

## Editing

Edit rows in the grid (`e`, `i`, `d`, then `:apply`). See [Editing rows](/docs/editing/).

- The table needs a primary key. Views cannot be edited.
- An update or delete must match exactly one row. None fails with "not found", more than one
  with a conflict, and nothing changes. An update that leaves the values as they were succeeds.
- An insert returns the stored key, including identity and default values.

## Transactions

`:tx begin` holds one session until you commit or roll back.

- The isolation levels are `read committed` and `serializable`.
- On a read-only connection the transaction is `SET TRANSACTION READ ONLY`, and its statements
  pass the read-only guard.
- In a writable transaction a statement must start with `SELECT`, `WITH`, `(`, `INSERT`,
  `UPDATE`, `DELETE`, `MERGE`, `LOCK`, `SAVEPOINT` or `ROLLBACK TO`. DDL (which commits
  implicitly), `COMMIT`, `ROLLBACK`, `SET TRANSACTION`, PL/SQL and procedure calls are refused.
  Use `:tx commit` and `:tx rollback`.
- If a statement ends the transaction anyway, the transaction is marked broken and the commit
  rolls back and says why.
- Run one statement at a time.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, or when you
run `:readonly on`. Oracle commits implicitly before DDL, and PL/SQL can commit, so a read-only
transaction alone is not enough:

1. **Allow-list, before anything is sent.** The statement must be one `SELECT`, `WITH` or
   bracketed query. tobira refuses it if, outside strings and quoted names, it holds a write or
   DDL word, a PL/SQL word (`BEGIN`, `DECLARE`, `FUNCTION`, `PROCEDURE`), `CALL`, `EXECUTE`,
   `FOR UPDATE`, `INTO`, `NEXTVAL`, a database link (`@`), or a side-effect package: any name
   starting with `DBMS_`, `UTL_`, `OWA_` or `APEX_`, and `HTP`, `HTF`, `HTTPURITYPE`,
   `DBURITYPE`, `XDBURITYPE`, `URIFACTORY`. Text it cannot read reliably is refused too:
   non-ASCII characters outside strings and quoted names, an unterminated string or comment, or a
   malformed number such as `1delete`.
2. **Read-only transaction.** The statement runs inside `SET TRANSACTION READ ONLY`. tobira
   checks afterwards that the statement did not end the transaction, then rolls back.

Refused statements make `tobira query` exit with code 4. Row edits are refused too.

- Quote a column whose name is one of the refused words, and quote non-ASCII names such as
  `"Café"`.
- The guard is only trusted on Oracle Database 12c to 23ai. On another version a read-only
  connection fails with `read-only access needs Oracle Database 12c to 23ai`. Writable
  connections work on any version.

**What it cannot stop.** A function called from a query, including a pipelined one in
`TABLE(f())`, can write through an autonomous transaction. Packages without the refused prefixes
(some `CTXSYS` and `WMSYS` packages) can act outside the transaction, and a synonym can hide a
`DBMS_` package or a remote table under another name. For real isolation, use a user with only
`SELECT` privileges, without `CREATE DATABASE LINK` and without `EXECUTE` on side-effect packages.

## Types

| Oracle | tobira type |
| --- | --- |
| `NUMBER(p)` with p up to 18, `INTEGER` | integer |
| `NUMBER(p,s)` with s > 0, `NUMBER(p)` with p > 18 | decimal |
| `NUMBER`, `FLOAT`, expressions | decimal; whole values that fit are integers |
| `BINARY_FLOAT`, `BINARY_DOUBLE` | float |
| `VARCHAR2`, `NVARCHAR2`, `CHAR`, `NCHAR`, `ROWID`, `INTERVAL ...` | text |
| `CLOB`, `NCLOB`, `LONG` | text (first 1 MiB) |
| `RAW` | bytes |
| `BLOB`, `LONG RAW` | bytes (first 1 MiB) |
| `DATE`, `TIMESTAMP`, `... WITH LOCAL TIME ZONE` | time (UTC) |
| `TIMESTAMP WITH TIME ZONE` | time, keeping its zone |
| `JSON` (21c and later) | JSON (first 1 MiB) |
| `BOOLEAN` (23ai) | boolean when browsing; 1 or 0 in query results |
| `VECTOR` (23ai) | vector |
| `BFILE`, `XMLTYPE`, object types | shown as their raw form |

A LOB value longer than 1 MiB is cut, and the result warns which column was cut. A cut JSON value
shows as text.

## Limits

- One statement per query.
- LOB values are cut at 1 MiB.
- No `ANALYZE` plans; a read-only transaction cannot explain.
- Table sizes are not shown.
- go-ora can, in two rare cases, print a server message straight to the terminal, over the UI.

## Compatibility

Tested with Oracle AI Database 26ai Free 23.26. Read-only access is enabled for Oracle Database 12c
to 23ai; versions before 23.26 have not been tested.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `invalid DSN: use oracle://user@host:port/service` | Check the DSN. The path is the service name. |
| `unsupported option ...: the oracle driver takes its settings from DSN options` | Move the setting from `options:` into the DSN. |
| `a PASSWORD in connStr is not supported` | Remove it from `connStr` and use the connection's `password:`. |
| `read-only access needs Oracle Database 12c to 23ai` | Use a writable connection with a `SELECT`-only user. |
| ORA-01466 on a read-only query | The table was created or changed a few seconds ago. Retry after a moment. |
| `read-only violation` | The statement is outside the allow-list. Quote a column named like a refused word. |
