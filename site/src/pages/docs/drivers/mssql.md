---
layout: ../../../layouts/DocsLayout.astro
title: SQL Server
---

# SQL Server

The `mssql` driver connects to Microsoft SQL Server. It is built into the `tobira` binary and
uses `github.com/microsoft/go-mssqldb`. You can query, browse, edit rows, see estimated plans and
run transactions.

## Quick start

```sh
tobira query "sqlserver://reader@db.internal:1433?database=app&encrypt=true" "select sysutcdatetime()" --read-only
```

```yaml
connections:
  - name: erp
    dsn: sqlserver://app@db.internal:1433?database=erp&encrypt=true
    password: env:ERP_SQL_PASSWORD
```

## Connecting

| Form | Example |
| --- | --- |
| URL | `sqlserver://user@host:1433?database=app&encrypt=true` |
| URL with a named instance | `sqlserver://user@host/SQLEXPRESS?database=app` |
| Alias scheme | `mssql://user@host?database=app` |
| ADO (config file, `driver: mssql`) | `server=host;user id=user;database=app` |
| ODBC (config file, `driver: mssql`) | `odbc:server=host;user id=user;database=app` |

- In a URL the path is the **instance name**, not the database. Name the database with
  `?database=`.
- In the ODBC form a braced value (`password={a;b}`) may hold `;`.
- Other go-mssqldb parameters (`app name`, `connection timeout`, `dial timeout` and so on) pass
  through. tobira ignores `log`, `timezone` and `guid conversion`: it never lets go-mssqldb
  write to the terminal, reads times in UTC and formats GUIDs itself.
- The connection's `options:` are refused. Put settings in the DSN.
- tobira connects when it opens the connection, so a wrong host or password fails at once.

### Password

Keep the password out of the DSN. Use the connection's `password:` setting with a
[secret reference](/docs/secrets/), or `TOBIRA_CONN_<NAME>_PASSWORD`. Either one replaces the
DSN's password. Errors that could show it are redacted.

### TLS

TLS is set with go-mssqldb's parameters:

| Parameter | Effect |
| --- | --- |
| `encrypt=true` | Encrypt the whole session and check the server certificate. |
| `encrypt=strict` | TDS 8.0: encrypt from the first byte and check the certificate. |
| not set, `encrypt=false` | Only the login packet is encrypted. |
| `encrypt=disable` | No encryption at all. Use it only against a server on your machine. |
| `certificate=/path/ca.pem` | Check the server against this CA or certificate. |
| `TrustServerCertificate=true` | Accept any certificate. Use it only for a local test server. |

Use `encrypt=true` (or `strict`) for any server that is not on your machine. The
`allow_plaintext=1` rule of the HTTP drivers does not apply here.

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (the estimated plan; the statement never runs, so no `ANALYZE`) |
| Completion | yes (T-SQL keywords, schemas, tables, columns) |
| Read-only guard | yes |

`--output` and `:export` use tobira's own writers.

## Queries

Write T-SQL. Each query is one statement; parameters are `@p1`, `@p2` and so on.

```sh
tobira query erp "select top 10 * from sales.orders order by order_id desc"
tobira query erp --explain "select * from sales.orders where customer_id = 7"
```

- Each query starts from a clean session, so `USE`, `SET` and temp tables never reach the next
  query.
- Only the first result set of a batch is returned.
- A transaction a statement leaves open (`BEGIN TRAN` without `COMMIT`) is rolled back when its
  result closes. Use `:tx begin` for a transaction across statements.
- Rows affected counts only the statement's own rows, not rows written by triggers.

## Browsing

The sidebar lists schemas. A schema of the database you log in to is shown by its bare name
(`dbo`, `sales`); a schema of another database you can open is `database.schema`
(`archive.dbo`). Names holding `.`, `[` or `]` are bracket-quoted (`[my.db].dbo`). System
databases are shown only when you log in to one. At most 200 databases are listed.

Describing a table shows columns (identity columns as auto-increment, computed and `rowversion`
columns as generated), the primary key, clustered and nonclustered indexes, foreign keys and the
`MS_Description` comment. Columnstore indexes are not listed.

- `~` (contains) and `^=` (starts with) compare the column as `nvarchar(max)` with `LIKE`.
- A `!` native filter must keep its parentheses balanced and hold no `;`.
- Tables with a primary key page by key. A sort, a `float` key, or a key type SQL Server cannot
  compare (`text`, `xml`, `geography`) pages with `OFFSET ... FETCH`.
- NULLs sort last ascending and first descending.

## Explain

`--explain` and `:explain` show SQL Server's estimated plan (`SHOWPLAN_XML`) as a tree of
operators, with the table or index each one reads, its estimated cost and rows. The statement is
compiled, never run, so you can explain `INSERT`, `UPDATE`, `DELETE` and `MERGE` on a read-only
connection too.

You can explain one `SELECT`, `WITH`, `INSERT`, `UPDATE`, `DELETE` or `MERGE`. `SET`, `GO`,
`EXEC`, transaction control and DDL are refused. Parameters are planned as unknown values.

## Editing

Edit rows in the grid (`e`, `i`, `d`, then `:apply`). See [Editing rows](/docs/editing/).

- The table needs a primary key. Tables with triggers work.
- An identity column you leave unset (or set to NULL) is filled by the server. A value you supply
  is inserted with `IDENTITY_INSERT`.
- An update or delete must match exactly one row. None fails with "not found", more than one
  with a conflict, and nothing changes. An update that leaves the values as they were succeeds.

## Transactions

`:tx begin` holds one connection until you commit or roll back.

- The isolation levels are `READ UNCOMMITTED`, `READ COMMITTED`, `REPEATABLE READ`, `SNAPSHOT`
  and `SERIALIZABLE`.
- Inside the transaction tobira refuses `COMMIT`, `ROLLBACK`, `BEGIN TRAN`, `EXEC`, procedure
  calls, `GO`, `SET IMPLICIT_TRANSACTIONS` and several statements at once. Use `:tx commit` and
  `:tx rollback`.
- After every statement tobira checks that its transaction is still open and can commit. If not,
  only `:tx rollback` works.
- A read-only transaction uses the read-only guard, and its commit rolls back.
- Run one statement at a time.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, or when you
run `:readonly on`. SQL Server has no read-only transaction, and one batch can hold several
statements without a `;` (`SELECT 1 DELETE FROM t` is two statements). So tobira checks twice:

1. **Allow-list, before anything is sent.** Only one `SELECT`, `WITH ... SELECT` or bracketed
   query, or one of the `sp_help` catalog procedures (`sp_help`, `sp_helptext`, `sp_helpindex`,
   `sp_helpconstraint`, `sp_helpdb` and similar), is sent. Wherever they appear outside strings
   and comments, tobira refuses data changes, `SELECT ... INTO`, `OUTPUT`, `EXEC`, `SET`, `USE`,
   `GO`, `DECLARE`, transaction control, control flow (`IF`, `WHILE`, `WAITFOR`), `BULK`,
   `BACKUP`, `DBCC`, `OPENROWSET`, `OPENQUERY`, `OPENDATASOURCE`, any `xp_` procedure,
   `NEXT VALUE FOR` and locking hints such as `TABLOCKX`, `UPDLOCK` and `HOLDLOCK`. It also refuses
   text it cannot read the way SQL Server does, such as an unterminated string or comment.
2. **A transaction that is always rolled back.** The statement runs inside a transaction (under
   `SNAPSHOT` isolation when the database allows it). Afterwards tobira checks that the statement
   did not end it, and rolls back.

Refused statements make `tobira query` exit with code 4. Row edits are refused too. A word such as
`GET`, `MOVE` or `SEND` used as a column name must be bracket-quoted (`[get]`) on a read-only
connection.

On a read-only connection that names a database, tobira also asks for
`ApplicationIntent=ReadOnly`. That only routes you to a readable secondary of an availability
group; the guard does not rely on it.

**What it cannot stop.** A user-defined function called from a query runs with your login's
permissions and can call extended procedures or CLR code; the rollback undoes its table writes
but not effects outside the database. A four-part name (`srv.db.dbo.t`) reads through a linked
server under that server's login. For a hard guarantee, connect with a login that only has
`db_datareader`.

## Types

| SQL Server | tobira type |
| --- | --- |
| `bit` | boolean |
| `tinyint`, `smallint`, `int`, `bigint` | integer |
| `decimal`, `numeric`, `money`, `smallmoney` | decimal (exact) |
| `float`, `real` | float |
| `date`, `datetime`, `datetime2`, `smalldatetime`, `datetimeoffset` | time in UTC (an offset is applied, not kept) |
| `time` | text (`15:04:05.1234567`) |
| `char`, `varchar`, `nchar`, `nvarchar`, `text`, `ntext`, `xml`, `sysname` | text |
| `uniqueidentifier` | text (`01234567-89AB-...`) |
| `binary`, `varbinary`, `image`, `rowversion`, `geography`, `geometry`, `hierarchyid` | bytes |
| `json` (SQL Server 2025) | JSON |
| `sql_variant` | the type of the value it holds |

SQL Server 2022 has no JSON type, so JSON stored in `nvarchar` columns shows as text.

## Limits

- One statement per query; only the first result set of a batch is shown.
- No `ANALYZE` plans.
- Under `SNAPSHOT`, a read-only query that reaches another database without snapshot isolation
  fails with error 3952.
- A read-only query through a linked server may need MSDTC.

## Compatibility

Tested with SQL Server 2022. Not yet tested: Azure SQL Database, Azure SQL Edge and SQL Server
2025.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `invalid DSN: use sqlserver://user@host?database=db ...` | Check the DSN form. In a URL, the database goes in `?database=`. |
| `unsupported option ...: the mssql driver takes its settings from the DSN` | Move the setting from `options:` into the DSN. |
| A certificate error | The server's certificate is not trusted. Add `certificate=/path/ca.pem`, or for a local test server `TrustServerCertificate=true`. |
| `read-only violation` | The statement is outside the allow-list. Bracket-quote column names such as `[get]`, or use a writable connection. |
