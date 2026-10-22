---
layout: ../../../layouts/DocsLayout.astro
title: "Troubleshooting: read-only refusals"
---

# Troubleshooting: read-only refusals

A write refused as read-only exits with code 4. This page explains why a statement was refused and
what to do, including the driver-specific cases where a read is refused too.

## A write is refused as read-only

```text
error: sqlite: query: only SELECT, WITH, VALUES, EXPLAIN and read-only PRAGMA statements are allowed on a read-only connection: read-only violation
```

Exit code 4 means the [read-only guard](/docs/security/read-only/) refused the statement
before it could write. That is the guard working: the connection has `read_only: true`, the command
ran with `--read-only`, or the TUI has `:readonly on`.

- To write, use a connection without `read_only: true` and run without `--read-only`.
  `:readonly off` cannot lower protection that the config file or the flag set.
- The statement checks read the text of the query, and refuse anything they cannot recognise as a
  read, so some reads are refused too. Each driver page lists what its guard accepts.
- **MySQL** runs the guard only on MySQL 8.0 to 8.4; on any other server, read-only queries are
  refused.
- **Redis** refuses read-only queries when the ACL denies `COMMAND`, which the guard uses to verify
  that a command really is a read.
- **MongoDB** accepts only the read commands on its allow-list; an aggregation with `$out` or
  `$merge` is a write.
- **SingleStore** has no read-only transaction, so every read-only connection, `--read-only` call
  and read-only transaction is refused as unsupported. Connect as a user with `SELECT` grants only.
- **MariaDB** fails a read-only call as unsupported when the session's `sql_mode` holds `MSSQL`,
  `ORACLE`, `POSTGRESQL`, `DB2`, `MAXDB` or an unknown flag, because the guard would read the text
  differently from the server.
- **Allow-listed functions.** The Vertica, Cassandra and Milvus guards refuse a function they do
  not know on a read-only path, user-defined ones included (Cassandra: write
  `system.abs(x)` when a user function shadows a built-in). Run it on a writable connection.
- **Refusals that protect your data from outside services.** On a read-only connection Elasticsearch refuses inference keys, Weaviate
  refuses `nearText` and `hybrid` with a remote vectorizer and `generate`, `ask` and `rerank`, and
  Milvus refuses a TEXTEMBEDDING text search and the `model` reranker. Search by vector instead:
  `S` or `:similar` embeds the text with your own `ai.embedder`.
- **Milvus "not loaded".** A read-only connection never loads a collection, so querying an
  unloaded one fails as unsupported, and the collection list marks it `not loaded`. Open it once
  on a writable connection, or load it with another client.
- **System indexes.** Elasticsearch and OpenSearch refuse `.`-prefixed, hidden and system indexes
  on read-only paths unless the connection sets `allow_system_indices=1`.
- **Keywords as names.** BigQuery, DynamoDB and Snowflake refuse a write word anywhere outside
  quotes, so quote a column named like one (`` `update` `` in BigQuery, `"set"` in DynamoDB).
- The AI assistant and the MCP server always query read-only, and only on drivers that declare the
  read-only guard; a write over MCP needs `mcp.allow_writes`, and in the TUI `:ai execute on`.
