---
layout: ../../../layouts/DocsLayout.astro
title: SQLite
---

# SQLite

The `sqlite` driver opens SQLite database files. It is built into the `tobira` binary and uses the
pure-Go `modernc.org/sqlite`, so it needs no C compiler and no system SQLite library. You can
query, browse, edit rows, explain queries and run transactions.

## Quick start

```sh
tobira query sqlite:./app.db "select * from users limit 5"
tobira connect sqlite:./app.db
```

A connection in the config file:

```yaml
connections:
  - name: app
    dsn: sqlite:./app.db
```

## Connecting

| Form | Example |
| --- | --- |
| `sqlite:<path>` | `sqlite:./app.db` |
| `sqlite3:<path>` | `sqlite3:/var/data/app.db` |
| `sqlite://<path>` | `sqlite:///var/data/app.db` (absolute), `sqlite://app.db` (relative) |
| `file:<path>` | `file:app.db`, `file:///var/data/app.db` |

- Relative paths are resolved against the directory you start tobira in.
- `~` is not expanded. Use `./` or an absolute path.
- On the command line, `tobira query` and `tobira connect` treat an argument as a DSN when it
  starts with `sqlite:` or `file:` or contains `://`. Use `sqlite3:` only in the config file.
- A writable connection creates the file if it does not exist. A read-only connection never
  creates it: a missing file fails with `database file not found`.

### DSN parameters

| Parameter | Effect |
| --- | --- |
| `mode=ro` | Open the connection read-only. |
| `mode=rw`, `mode=rwc` | Accepted; the connection stays writable. |

Any other parameter is refused with `unsupported DSN parameter`. In-memory databases
(`:memory:`, `file::memory:`, `mode=memory`) are not supported: open a file instead.

### Options

| Option | Default | Effect |
| --- | --- | --- |
| `busy_timeout` | `5000` | Milliseconds to wait for a lock held by another process before failing. |

```yaml
connections:
  - name: app
    dsn: sqlite:./app.db
    options:
      busy_timeout: "10000"
```

Writable connections also turn on foreign key enforcement (`PRAGMA foreign_keys = ON`).

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes |
| Edit rows | yes |
| Transactions | yes (serializable only) |
| Explain | yes (`EXPLAIN QUERY PLAN`; no `EXPLAIN ANALYZE`) |
| Completion | yes (keywords, databases, tables, columns) |
| Read-only guard | yes |

## Queries

Write SQLite SQL. Each query runs exactly one statement; a second statement is refused as
invalid.

```sh
tobira query sqlite:./app.db "select name, email from users where active = 1"
tobira query sqlite:./app.db --explain "select * from orders where user_id = 7"
tobira query sqlite:./app.db --format csv --output users.csv "select * from users"
```

For a statement that returns no rows, tobira reports the number of rows it changed. That count
includes rows changed by triggers.

## Browsing

The sidebar lists every attached database (`main` first), then its tables and views. Row counts
are not shown. Describing a table shows its columns (generated columns too), primary key,
indexes, foreign keys and the `CREATE` statement.

- `~` (contains) and `^=` (starts with) use `LIKE`, so they ignore case for ASCII letters:
  `name ~ 'abc'` also matches `ABC`.
- Pages are fetched with `OFFSET` and ordered by your sort, then by the primary key. A table
  without a primary key is ordered by its `rowid`.
- A `!` native filter is the body of a `WHERE` clause and goes through the read-only guard.

## Editing

Edit rows in the grid (`e`, `i`, `d`, then `:apply`). See [Editing rows](/docs/editing/).

- The table needs a primary key. Views cannot be edited.
- Each update or delete must match exactly one row. If it matches none, it fails with
  `no row matches the key`. If it matches more than one, it is undone and fails with a conflict.
- An `INTEGER PRIMARY KEY` column is filled by SQLite when you leave it unset on insert.

## Transactions

`:tx begin` starts a transaction with `BEGIN IMMEDIATE`. If another process holds the write lock,
the begin fails with a conflict instead of waiting until your first write.

- SQLite transactions are serializable. Asking for another isolation level fails.
- Inside the transaction, `COMMIT`, `END`, `BEGIN` and `ROLLBACK` are refused. Use `:tx commit`
  and `:tx rollback`. `SAVEPOINT`, `RELEASE` and `ROLLBACK TO` are allowed.
- If a statement ends the transaction anyway, every later statement fails with
  `a statement ended the transaction; roll back`.
- Tables you create inside the transaction can be edited and explained inside it.
- `PRAGMA` settings and temp tables made inside a transaction are dropped when it ends.
- On a read-only connection the transaction is read-only and follows the read-only rules below.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, when you
use `:readonly on`, or when the DSN has `mode=ro`. On a read-only connection:

- SQLite opens the file read-only, and `PRAGMA query_only` is on for every statement.
- tobira only runs statements that start with `SELECT`, `WITH`, `VALUES`, `EXPLAIN`, or a pragma
  that reads (such as `PRAGMA table_info(users)` or `PRAGMA user_version`).
- It refuses writes, DDL, `ATTACH`, `DETACH`, `VACUUM`, `REINDEX`, `ANALYZE`, any `PRAGMA x = ...`,
  and pragmas with side effects such as `optimize` and `wal_checkpoint`. Refused statements never
  reach SQLite, and `tobira query` exits with code 4.
- Row edits are refused.

**What it cannot stop.** The guard protects the database through this connection only. It does
not stop another program from writing to the file. A `WITH ... INSERT` passes the first check,
but SQLite refuses it because the file is open read-only.

`--explain` always runs on a read-only handle, even on a writable connection, so explaining a
statement never changes the database. It can explain `INSERT`, `UPDATE`, `DELETE` and `REPLACE`,
because SQLite only plans them. Outside a transaction, `--explain` therefore does not see temp
tables or attached databases that earlier statements created; inside a transaction, it does.

## Types

Values are converted by the column's declared type.

| Declared type | tobira type |
| --- | --- |
| INTEGER, INT, BIGINT, SMALLINT | integer |
| REAL, DOUBLE, FLOAT | float |
| DECIMAL(p,s), NUMERIC(p,s) | decimal, shown with scale `s` (12.5 shows as `12.50`) |
| BOOLEAN, BOOL | boolean from 0 and 1 (other integers stay integers) |
| DATE, DATETIME, TIMESTAMP | time, from text or Unix seconds (UTC when no zone); text that does not parse stays text |
| JSON | JSON when valid, otherwise text |
| BLOB | bytes |
| TEXT, VARCHAR(n), CHAR | text |
| no declared type (expressions) | taken from the first row: integer, float, bytes or text |

NULL is always NULL.

## Limits

- One statement per query.
- No in-memory databases.
- No `EXPLAIN ANALYZE`.
- Row counts are not shown in the sidebar.
- A table without a primary key whose `rowid`, `_rowid_` and `oid` names are all taken by real
  columns has no stable order, so rows that tie on your sort can repeat or be skipped across pages.

## Compatibility

Built with `modernc.org/sqlite` 1.60. Any SQLite 3 database file works.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `database file not found` | The path is wrong, or the file does not exist and the connection is read-only. Check the path; a relative path is resolved from where you started tobira. |
| `unsupported DSN parameter` | Remove the parameter. Only `mode=ro`, `mode=rw` and `mode=rwc` are accepted. |
| `in-memory databases are not supported` | Use a database file. |
| `database is locked` | Another process holds a lock. Wait, or raise the `busy_timeout` option. |
| `read-only violation` | The connection is read-only. Drop `--read-only`, `read_only: true` or `mode=ro`, or run `:readonly off`. |
| `transaction control statements are not allowed inside a transaction` | Use `:tx commit` or `:tx rollback`. |
