---
layout: ../../../../layouts/DocsLayout.astro
title: "PostgreSQL: queries, browsing and editing"
---

# PostgreSQL: queries, browsing and editing

How the [PostgreSQL driver](/docs/drivers/postgres/) runs SQL, browses tables, explains queries,
edits rows and handles transactions. Everything here also applies to
[CockroachDB](/docs/drivers/cockroachdb/) and [Greenplum](/docs/drivers/greenplum/);
[Amazon Redshift](/docs/drivers/redshift/) has no editing or transactions.

## Queries

Write PostgreSQL SQL. Each query is one statement.

```sh
tobira query app "select id, email from users where created_at > now() - interval '1 day'"
tobira query app --limit 50 "select * from events order by id desc"
tobira query app --explain "select * from orders where user_id = 7"
```

- **Row limit.** With a limit (`--limit`, or the editor's `:set limit`), a `SELECT`, `WITH`,
  `VALUES` or `TABLE` reads through a server-side cursor, so the server never builds more rows
  than you asked for. The result says when it was cut.
- **Stopping early.** Closing a half-read result cancels the statement on the server instead of
  reading the rest.
- **Rows affected.** For a statement that returns no rows, tobira reports the count from the
  server's reply.
- **Column types.** The result header shows each column's PostgreSQL type, including enums and
  extension types.

## Browsing

The sidebar lists schemas (without `pg_catalog`, `information_schema`, `pg_toast*` and
`pg_temp_*`), then tables, views, materialized views and partitioned tables, with an estimated
row count (unknown until the table is analyzed). Describing a table shows:

- columns with type, nullability, default, generated flag and comment; `serial` and identity
  columns are marked auto-increment;
- the primary key, indexes (with their method) and foreign keys;
- the table's total size and comment.

The description has no `CREATE` statement.

Filtering and sorting:

- `~` (contains) and `^=` (starts with) compare the value as text with `LIKE`, so they are
  case-sensitive.
- A `!` native filter is the body of a `WHERE` clause. Use a read-only connection when it calls
  functions, so a function with side effects runs in a read-only transaction.
- Ascending sorts put NULLs last; descending sorts put them first.

Paging:

- With a primary key and no sort, pages follow the key (`WHERE pk > last`), which stays fast deep
  into a large table.
- With a sort, or without a primary key, pages use `OFFSET`, ordered by the sort and then the key.
- A view without a sort has no stable order. A partitioned table without a primary key can repeat
  or skip rows across pages; give it a primary key or sort on a unique column.

## Explain

`--explain` and `:explain` run `EXPLAIN (FORMAT JSON)` and show the plan as a tree (node type,
relation, index, join type, cost and estimated rows) together with the usual text plan. Planning
never runs the statement.

An analyzed explain runs `EXPLAIN (ANALYZE, FORMAT JSON)`, which does run the statement. tobira
always rolls it back, even on a writable connection. The rollback cannot undo what happens
outside the transaction: sequence values are used up, and writes made through `dblink` stay. On a
read-only connection, analyzing a statement that writes is refused.

Inside a transaction, `:explain` runs on the transaction, so it sees the transaction's changes. It
runs inside a savepoint that is rolled back, so even an analyzed explain changes nothing, and a
failed explain does not abort the transaction.

## Editing

Edit rows in the grid (`e`, `i`, `d`, then `:apply`). See [Editing rows](/docs/editing/).

- The table needs a primary key. Views and materialized views cannot be edited.
- Each update or delete must match exactly one row. None fails with `no row matches the key`;
  more than one is rolled back and fails with `the key matches N rows; nothing was changed`.
- An insert returns the stored key, including keys the server generated.
- Each change runs in its own savepoint, so one failing change does not abort an open
  transaction.

## Transactions

`:tx begin` holds one connection until you commit or roll back.

- The isolation levels are `read uncommitted`, `read committed`, `repeatable read` and
  `serializable`.
- On a read-only connection the transaction is `BEGIN READ ONLY` and takes its snapshot at once.
- `COMMIT`, `END`, `ROLLBACK`, `ABORT`, `BEGIN`, `START` and `PREPARE TRANSACTION` are refused
  inside the transaction. Use `:tx commit` and `:tx rollback`.
- If a statement ends the transaction anyway, tobira closes that connection and the statement
  fails with `the statement ended the transaction; the connection was discarded`.
- After an error, PostgreSQL refuses every statement until you roll back. tobira says so:
  `the transaction may be aborted; :tx rollback to continue`.
- `SELECT ... INTO` creates a table, so it runs in a writable transaction and is refused in a
  read-only one.

When you close the connection or quit, every open transaction is rolled back.
