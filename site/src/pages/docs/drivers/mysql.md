---
layout: ../../../layouts/DocsLayout.astro
title: MySQL
---

# MySQL

The `mysql` driver connects to MySQL and Percona Server. It is built into the `tobira` binary and
uses `github.com/go-sql-driver/mysql`. You can query, browse, edit rows, explain queries and run
transactions. The same driver also serves [MariaDB](/docs/drivers/mariadb/) and
[SingleStore](/docs/drivers/singlestore/).

## Quick start

```sh
tobira query "mysql://reader@db.internal:3306/app?tls=true" "select now()" --read-only
```

```yaml
connections:
  - name: shop
    dsn: mysql://app@db.internal:3306/shop?tls=true
    password: env:SHOP_MYSQL_PASSWORD
```

## Connecting

```text
mysql://user@localhost:3306/app
mysql://user@db.internal/app?tls=true
mysql://user@localhost/app?socket=/var/run/mysqld/mysqld.sock
user@tcp(localhost:3306)/app
```

- The port defaults to 3306. Put IPv6 hosts in brackets.
- In the URL form, the user and password run to the last `@` before any `?`, so a password may
  contain `@`, `/` and `:`. Write `?` as `%3F`.
- `socket=<path>` connects through a Unix socket.
- The last form is go-sql-driver's own. It has no scheme, so use it in the config file with
  `driver: mysql`.
- tobira connects when it opens the connection, so a wrong host or password fails at once.

### Password

Keep the password out of the DSN. Use the connection's `password:` setting with a
[secret reference](/docs/secrets/), or `TOBIRA_CONN_<NAME>_PASSWORD`. Either one replaces the
DSN's password. Errors never show it.

### TLS

TLS is off unless you ask for it. Set go-sql-driver's `tls` parameter:

| `tls` | What happens |
| --- | --- |
| not set, `false` | No TLS. |
| `preferred` | TLS if the server offers it, without checking the certificate. |
| `skip-verify` | TLS, without checking the certificate. |
| `true` | TLS, and the certificate must be trusted by your system and match the host. |

Use `tls=true` for any server that is not on your machine. The `allow_plaintext=1` rule of the
HTTP drivers does not apply here.

### Parameters

go-sql-driver's parameters work (`tls`, `timeout`, `readTimeout` and so on). Any other key is set
as a session variable. The connection's `options:` are refused: put settings in the DSN.

tobira also enforces these:

| Setting | Rule |
| --- | --- |
| `multiStatements` | Must be off. tobira runs one statement per query. |
| `interpolateParams` | Must be off, so the server runs exactly the text tobira checked. |
| `charset`, `collation` | Only `utf8mb4`. Other charsets and the `names`, `character_set_*` and `collation_*` variables are refused, because some multi-byte charsets could fool the read-only guard. |
| Time zone | Fixed to UTC. `DATETIME` and `TIMESTAMP` values come back in UTC. |
| `flavor` | `mysql`, `mariadb` or `singlestore` forces the server family. See [MariaDB](/docs/drivers/mariadb/) and [SingleStore](/docs/drivers/singlestore/). |

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (`EXPLAIN FORMAT=JSON` as a tree; never `ANALYZE`) |
| Completion | yes (keywords, databases, tables, columns) |
| Read-only guard | yes, on MySQL 8.0 to 8.4 |

`--output` and `:export` use tobira's own writers.

## Queries

Write MySQL SQL. Each query is one statement; a trailing `;` is fine.

```sh
tobira query shop "select id, total from orders order by id desc limit 10"
tobira query shop --explain "select * from orders where customer_id = 7"
```

Each query runs on a fresh connection that is closed afterwards, on writable connections too. So
nothing a query does to its session (`USE`, `SET`, user variables, `GET_LOCK`, `LOCK TABLES`, an
open transaction) reaches the next query. Use `:tx begin` when you need several statements in one
session.

## Browsing

The sidebar lists databases (without `information_schema`, `mysql`, `performance_schema` and
`sys`), then tables and views. Row estimates come from `information_schema` and can be stale.
Describing a table shows columns (with defaults, generated expressions and `AUTO_INCREMENT`),
the primary key, indexes and foreign keys.

- `~` (contains) and `^=` (starts with) compare the value as text with `LIKE`.
- A `!` native filter is the body of a `WHERE` clause. It must keep its parentheses balanced and
  hold no `;`. On a read-only connection the guard checks it.
- Tables with a primary key page by key, which stays fast on deep pages. With a sort, without a
  primary key, or when the key has a `FLOAT` column, pages use `OFFSET`. A table without a
  primary key is ordered by every column.
- NULLs sort last ascending and first descending.
- A `TINYINT(1)` column shows as a boolean when browsing. In query results it stays a number.

## Explain

`--explain` and `:explain` show `EXPLAIN FORMAT=JSON` as a tree and `EXPLAIN FORMAT=TREE` as text
(the classic `EXPLAIN` table for single-table updates and deletes). Explaining never runs the
statement; `ANALYZE` is not supported. You can explain one `SELECT`, `WITH`, `VALUES`, `TABLE`,
`INSERT`, `REPLACE`, `UPDATE` or `DELETE`.

On a read-only connection MySQL refuses to explain `INSERT`, `REPLACE`, `UPDATE` and `DELETE`, so
those fail as read-only and nothing runs.

## Editing

Edit rows in the grid (`e`, `i`, `d`, then `:apply`). See [Editing rows](/docs/editing/).

- The table needs a primary key. Views cannot be edited.
- An update or delete must match exactly one row. None fails with "not found", more than one
  with a conflict, and nothing changes. An update that leaves the values as they were succeeds.
- An `AUTO_INCREMENT` key you leave unset is filled by the server, and tobira shows the new value.

## Transactions

`:tx begin` holds one connection until you commit or roll back.

- The isolation levels are `read uncommitted`, `read committed`, `repeatable read` and
  `serializable`.
- On a read-only connection the transaction is read-only and its statements pass the read-only
  guard.
- MySQL commits implicitly on some statements, which would end the transaction behind tobira's
  back. In a writable transaction tobira refuses them: DDL (`CREATE`, `ALTER`, `DROP`, `RENAME`,
  `TRUNCATE`), `GRANT`, `REVOKE`, `COMMIT`, `ROLLBACK`, `START TRANSACTION`, `BEGIN`, savepoints,
  `LOCK TABLES`, `SET autocommit`, maintenance statements (`ANALYZE`, `OPTIMIZE`, `REPAIR`,
  `FLUSH`), and `CALL`, `PREPARE` and `EXECUTE`. Use `:tx commit` and `:tx rollback`.
- If a statement ends the transaction anyway, the transaction is marked broken: later statements
  and commit fail, and only rollback works. A deadlock that rolled the transaction back is
  reported as a conflict.
- Run one statement at a time. Closing a result early reads at most 1000 more rows (or a quarter
  of a second), then stops the statement with `KILL QUERY` from a second connection; the
  transaction stays open.
- `KILL QUERY` needs a direct connection to MySQL. Behind a multiplexing proxy such as ProxySQL,
  tobira cannot stop the statement, so it closes the connection and the transaction is lost.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, or when you
run `:readonly on`. MySQL ends a transaction implicitly on many statements, so tobira does not
rely on a read-only transaction alone:

1. **Allow-list.** Only `SELECT`, `WITH`, `VALUES`, `TABLE`, `SHOW`, `DESCRIBE`, `EXPLAIN` of a
   query and `USE` are sent. Executable comments (`/*! ... */`), `SELECT ... INTO OUTFILE` or
   `DUMPFILE`, `WITH ... UPDATE`, `EXPLAIN ANALYZE`, several statements, and text tobira cannot
   read to its end are refused before anything is sent.
2. **Read-only transaction.** Each call runs in a read-only session inside
   `START TRANSACTION READ ONLY ... ROLLBACK`. `FOR UPDATE` is refused there by MySQL.
3. **Check afterwards.** tobira checks that the statement is still inside that transaction and
   that the session is still read-only. If not, the call fails as read-only.

Refused statements make `tobira query` exit with code 4. Row edits are refused too.

The guard is only trusted on MySQL 8.0 to 8.4. On any other version (MySQL 5.7, MySQL 9 or later)
a read-only connection fails to open with `read-only access needs MySQL 8.0 to 8.4`. Writable
connections work on any version.

**What it cannot stop.** A `FEDERATED` table sends statements to another server when it is read;
those run outside the local read-only transaction. Grants are the real boundary: for untrusted
work, connect as a user with only `SELECT` grants.

## Types

| MySQL | tobira type |
| --- | --- |
| TINYINT ... BIGINT, YEAR, BIT | integer (BIGINT UNSIGNED and BIT values above the integer range: decimal) |
| TINYINT(1) | boolean when browsing; integer in query results |
| DECIMAL | decimal |
| FLOAT, DOUBLE | float |
| DATE, DATETIME, TIMESTAMP | time (UTC) |
| TIME | text (it can exceed 24 hours) |
| JSON | JSON |
| CHAR, VARCHAR, TEXT, ENUM, SET | text |
| BINARY, VARBINARY, BLOB, spatial types | bytes |

## Limits

- One statement per query.
- Every query opens a new connection, which adds a round trip of connecting and signing in.
- No `EXPLAIN ANALYZE`.
- No native export and no watch.

## Compatibility

Tested with MySQL 8.4. Read-only access is supported on MySQL 8.0 to 8.4.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `authentication failed` | Check the user and the password reference (`tobira doctor` shows whether it resolves). |
| `connection failed` | Check the host, port, `socket` and `tls`. |
| `read-only access needs MySQL 8.0 to 8.4` | The server version is outside the verified range. Use a writable connection with a `SELECT`-only user. |
| `unsupported option ...: the mysql driver takes its settings from DSN parameters` | Move the setting from `options:` into the DSN. |
| `multiStatements=true is not supported` | Remove the parameter. |
| `... is not supported: tobira pins the client charset to utf8mb4` | Remove the `charset` or `collation` parameter. |
| `read-only violation` | The statement is outside the allow-list, or the connection is read-only. |
