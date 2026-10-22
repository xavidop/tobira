---
layout: ../../../../layouts/DocsLayout.astro
title: "PostgreSQL: read-only guard"
---

# PostgreSQL: read-only guard

A [PostgreSQL](/docs/drivers/postgres/) connection is read-only when it has `read_only: true`,
when you pass `--read-only`, or when you run `:readonly on`. This page says what tobira then
refuses, and what it cannot stop. [CockroachDB](/docs/drivers/cockroachdb/#read-only-guard) and
[Amazon Redshift](/docs/drivers/redshift/#read-only-guard) add an allow-list on top; see their pages.

## How it protects you

- **Every call is its own read-only transaction.** Each query runs inside
  `BEGIN READ ONLY ... ROLLBACK`, whatever the statement does. Text such as `COMMIT`,
  `BEGIN READ WRITE` or `SET default_transaction_read_only = off` cannot affect the next call,
  because the next call opens a new read-only transaction. A write fails with the server's
  read-only error.
- **One statement per query.** tobira always sends your text in a way that makes the server
  reject a second statement.
- **Read-only session default.** A read-only connection also starts its sessions with
  `default_transaction_read_only=on`. Neither the DSN nor `options:` can turn it off.
- **Clean sessions.** Every connection is reset (`DISCARD ALL`) before it is reused, so temp
  tables, `SET`, `SET ROLE`, advisory locks and prepared statements never carry over to the next
  query. This also applies to writable connections.
- **Schema reads are read-only too.** Listing schemas, tables and columns always runs in a
  read-only transaction.

## What it refuses

These are refused before they reach the server, and `tobira query` exits with code 4:

| Refused | Why |
| --- | --- |
| Any statement starting with `COPY` | `COPY ... TO '<file>'` and `TO PROGRAM` write on the server even in a read-only transaction. |
| `DO` blocks | They can build and run SQL that no check sees. |
| `lo_export`, `lo_import`, `lo_unlink`, `pg_file_write`, `pg_file_rename`, `pg_file_unlink`, `pg_logdir_ls` | These read or write server files. They are refused wherever they appear in the text, in any quoting. |
| Changing `standard_conforming_strings`, `backslash_quote` or `escape_string_warning` | Changing how strings are read could hide one of the refused names. |
| `PREPARE TRANSACTION` | It would leave the transaction behind on the server. |
| `COMMIT AND CHAIN`, `ROLLBACK AND CHAIN` and similar | They would start a new transaction outside tobira's control. |
| `SELECT ... INTO` | It creates a table. |

Row edits are refused too.

## What it cannot stop

The read-only guard protects you from accidental writes. It is not a sandbox against a powerful
role.

- A superuser can still reach server programs or files through a function that builds SQL at
  run time, such as `EXECUTE 'COPY ...'` inside a function body.
- `dblink` and `postgres_fdw` can write through another session.
- `LOCK TABLE` can still take locks that block other sessions.

For untrusted use, connect with a least-privilege role: `GRANT SELECT` only, no
`pg_write_server_files`, no `pg_execute_server_program`, and no `EXECUTE` on `dblink`.

## Related behaviour

- [pgvector](/docs/drivers/pgvector/#read-only-guard) similarity searches always run read-only, on
  writable connections too.
- The [native csv export](/docs/drivers/postgres/listen-and-export/#native-csv-export) always runs
  read-only and applies the refusals above on every connection.
- An analyzed `EXPLAIN` is always rolled back; on a read-only connection, analyzing a statement
  that writes is refused.
