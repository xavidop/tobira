---
layout: ../../../layouts/DocsLayout.astro
title: MariaDB
---

# MariaDB

The `mariadb` driver connects to MariaDB. It is built into the `tobira` binary and shares the
[MySQL driver](/docs/drivers/mysql/)'s code, so everything on that page about connecting, TLS,
browsing, editing, transactions and completion applies. This page lists what differs.

## Quick start

```sh
tobira query "mariadb://reader@db.internal:3306/app?tls=true" "select now()" --read-only
```

```yaml
connections:
  - name: maria
    dsn: mariadb://app@db.internal:3306/app?tls=true
    password: env:MARIA_PASSWORD
```

## Connecting

| DSN | Result |
| --- | --- |
| `mariadb://user@host:3306/db` | MariaDB, through the `mariadb` driver |
| `mysql://user@host:3306/db?flavor=mariadb` | the same, through the `mysql` driver |
| `mysql://user@host:3306/db` | tobira asks the server when it connects and detects MariaDB |
| `user@tcp(host:3306)/db` with `driver: mariadb` | MariaDB, in go-sql-driver's form |

tobira also detects a MariaDB whose version string was changed to look like MySQL.

## Capabilities

The same as [MySQL](/docs/drivers/mysql/#capabilities): query, browse, edit rows, transactions,
explain, completion and the read-only guard.

## Explain

MariaDB has no `FORMAT=TREE`. The plan tree comes from `EXPLAIN FORMAT=JSON` and the text plan is
the classic `EXPLAIN` table. `ANALYZE` is never used. On a read-only connection, explaining
`INSERT`, `UPDATE` or `DELETE` is refused by the server.

## Types and browsing

- `JSON` is an alias for `LONGTEXT` in MariaDB, so JSON columns show as text.
- `UUID`, `INET4` and `INET6` columns show as text.
- Literal defaults are shown as MariaDB stores them; a column without a default shows none.
- Indexes list their columns; expression key parts are not shown.

## Read-only guard

| | Supported versions |
| --- | --- |
| Writable connections | any MariaDB version |
| Read-only connections, `--read-only` calls, read-only transactions | MariaDB 11.1 to 11.x |

- MariaDB 10.x lacks the `transaction_read_only` variable, so a read-only connection fails with
  `read-only access needs a server with the transaction_read_only variable`.
- On MariaDB 12 and later, a read-only connection fails with
  `read-only access needs MariaDB 11.1 to 11.x`.
- Forcing `flavor=mysql` onto MariaDB, or `flavor=mariadb` onto MySQL, also turns read-only off.

The guard works as on [MySQL](/docs/drivers/mysql/#read-only-guard). MariaDB adds:

- `/*M! ... */` executable comments are refused like `/*! ... */`.
- Sequence functions (`NEXTVAL`, `NEXT VALUE FOR`, `SETVAL`) are refused by MariaDB inside the
  read-only transaction.
- `RETURNING`, compound statements (`BEGIN NOT ATOMIC`, `IF`, `WHILE`), `SET STATEMENT ... FOR`,
  `EXECUTE IMMEDIATE` and `ANALYZE SELECT` are outside the allow-list.

**sql_mode.** Some modes change how MariaDB reads quotes and brackets, which would make the guard
misread your text. On every read-only call and every transaction, tobira checks the session's
`sql_mode`. `ANSI`, `ANSI_QUOTES`, `NO_BACKSLASH_ESCAPES`, `TRADITIONAL` and the strict and
zero-date flags are fine. `MSSQL`, `ORACLE`, `POSTGRESQL`, `DB2`, `MAXDB` or an unknown flag make
the call fail as unsupported.

**What it cannot stop.** `CONNECT` tables with `EXECSRC`, `SPIDER` tables and
`FEDERATED`/`FEDERATEDX` tables send statements to another server when they are read. Those run
outside the local read-only transaction, so selecting from such a table can change the remote
side. Only an administrator can create them. On a server that has them, rely on the remote
account's grants too, or connect as a user with only `SELECT` grants.

## Compatibility

Tested with MariaDB 11.8.9.
