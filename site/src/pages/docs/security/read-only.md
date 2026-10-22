---
layout: ../../../layouts/DocsLayout.astro
title: "Security: read-only guard"
---

# Security: read-only guard

Connections are writable by default. `read_only: true` on a connection, `--read-only` on any
command, or `:readonly on` in the terminal UI turns on a hard guard that the query text cannot lift.
A refused write exits with code 4. This page explains how the guard works on each driver and what
it cannot stop.

## Quick start

```yaml
connections:
  - name: prod
    dsn: postgres://reader@db.example.com/app?sslmode=require
    password: env:PROD_PASSWORD
    read_only: true
```

```sh
tobira query prod "delete from orders" ; echo $?   # refused, exit code 4
```

## How the guard works

- The database session enforces read-only wherever the database allows it: a read-only
  transaction, a read-only file handle, a read-only setting.
- In front of that, a statement check refuses anything it does not recognise as a read. These
  checks read the statement's text, and what a check cannot recognise as a read is refused before
  it is sent.
- The AI assistant and the MCP server run their query tools only on connections whose driver has
  the guard, and always with the read-only flag.

The guard stops accidental writes. It is not a sandbox. A PostgreSQL superuser, for example, can
still reach server-side programs or files through functions that build SQL at run time. For
untrusted use, connect with a least-privilege role (on PostgreSQL, one without
`pg_execute_server_program` or `pg_write_server_files`) as the real boundary.

## Per-driver read-only layers

The last column names what the guard cannot stop, as each driver page documents it. A dash means
the page lists no such gap, not that there is none.

| Driver | Session layer | Statement layer | Does not stop |
|---|---|---|---|
| SQLite | a read-only handle with `query_only` | one statement: `SELECT`, `WITH`, `VALUES` or a read-only `PRAGMA`, optionally behind `EXPLAIN [QUERY PLAN]` | — |
| PostgreSQL | a read-only transaction per call and a read-only session default | one statement per query; `COPY`, `DO` and server-file functions such as `lo_export` refused, also in every export | a superuser reaching server programs or files through functions that build SQL at run time; `dblink` |
| MySQL | a read-only transaction per call, a post-check that the statement stayed inside it, a pinned `utf8mb4` charset | an allow-list of statement kinds; MySQL 8.0 to 8.4 only, each query on its own connection | — |
| MariaDB | MySQL's, plus a `sql_mode` check on every read-only call | MySQL's allow-list, plus `/*M! */` comments, sequences, `RETURNING`, compound statements and `EXECUTE IMMEDIATE` refused | `CONNECT`, `SPIDER` and `FEDERATED` tables, which run statements on a remote server |
| SingleStore | none: SingleStore has no read-only transaction | none: read-only connections and queries are refused | everything; use a `SELECT`-only user |
| CockroachDB | the PostgreSQL layers; `default_transaction_read_only=on` | an allow-list (`SELECT`, `WITH`, `VALUES`, `TABLE`, `SHOW`, `EXPLAIN` of those); `EXPORT`, `BACKUP`, `CREATE CHANGEFEED`, `SHOW BACKUP` and `crdb_internal` functions refused | — |
| Amazon Redshift | a read-only transaction per call | the same allow-list; `UNLOAD`, `COPY`, `CALL` and `SELECT … INTO` refused | — |
| Greenplum | the PostgreSQL layers | the PostgreSQL guard | external and web tables, which read (or run) what their definition names |
| SQL Server | a transaction that is always rolled back, under `SNAPSHOT` when allowed | one `SELECT`, `WITH … SELECT` or `sp_help…`; `EXEC`, `SET`, `GO`, `OPENROWSET`, `OPENQUERY`, `xp_…`, `BULK` and locking hints refused | user-defined functions reaching out through extended procedures or CLR; linked servers (four-part names) |
| Oracle | `SET TRANSACTION READ ONLY`, checked not to have ended, then rolled back | one `SELECT`, `WITH` or parenthesised query; PL/SQL, `CALL`, DDL, DML, `FOR UPDATE`, `NEXTVAL`, database links and `DBMS_*`, `UTL_*` and similar packages refused | user functions with autonomous transactions; synonyms hiding a package or a remote table; XML external entities on unpatched servers |
| Vertica | `START TRANSACTION READ ONLY`, always rolled back | one `SELECT`, `WITH`, `SHOW` or `EXPLAIN`; only allow-listed built-in functions (meta-functions refused); no `COPY … FROM LOCAL` | user-defined functions (UDx) inside a view or overloading a built-in name |
| Snowflake | `MULTI_STATEMENT_COUNT=1` (no read-only session exists) | one `SELECT`, `WITH`, `SHOW`, `DESCRIBE` or `EXPLAIN`; no `->>`, stage references, `CALL`, `COPY`, `SYSTEM$` functions or unknown table functions | user-defined and external functions called from a `SELECT` |
| BigQuery | a dry run whose statement type must be `SELECT`; `maximumBytesBilled` on every job | one `SELECT` or `WITH`; scripts, DML, DDL, `EXPORT` and `EXTERNAL_QUERY` refused | remote and ML functions called from a `SELECT` (they can send data to an outside model) |
| libSQL / Turso | `BEGIN … ROLLBACK` on a fresh stream; a read-only token if you use one | the SQLite allow-list, plus no write words | — |
| Cloudflare D1 | none (no read-only mode or transactions over REST) | the SQLite allow-list, no write words, one statement | — |
| MongoDB | none (MongoDB has no read-only session) | the command document is checked as sent against an allow-list (`find`, `aggregate`, `count`, `distinct`, the list and stats commands, `explain` of the first four); `$out`, `$merge`, session and write-concern fields refused | server CPU used by `$where` and `$function` |
| Cloud Firestore | none (no read-only protocol mode) | the request's keys are an allow-list of reads; no write verb can be expressed | — |
| Azure Cosmos DB | Cosmos SQL cannot write | one `SELECT`, no user-defined function (`udf.…`); stored procedures and triggers are never run | — |
| Amazon DynamoDB | none (no read-only session) | one PartiQL `SELECT`, no write verb, no Ion literal | — |
| Cassandra | none (no read-only session) | one `SELECT`, `DESCRIBE` or `DESC`; built-in functions only | cost: `ALLOW FILTERING` can scan a whole table |
| Redis | the server's own `COMMAND INFO` flags, so a renamed command is still refused | an allow-list of read commands; scripts, `MULTI`, `CONFIG`, `SELECT`, `KEYS` and every write refused | — |
| ClickHouse | `readonly=1` on every read-only query; for a user already read-only, the server's own parse of each query is checked | an allow-list (`SELECT`, `WITH`, `SHOW`, `DESCRIBE`, `EXPLAIN`, `EXISTS`), one statement, no `INTO OUTFILE`; outside-reaching table functions refused | server tables and dictionaries that read from outside (`URL`, `S3`, external `dictGet`) |
| Elasticsearch / OpenSearch | none (no read-only session) | a read allow-list of endpoints and parameters; no `script` key; system and hidden indexes refused; inference keys refused (they send data to an outside service) | — |
| InfluxDB | InfluxDB 3's query API cannot write | one `SELECT`, `WITH … SELECT`, `SHOW` or `EXPLAIN` without `INTO` | — |
| Qdrant | queries never write, on any connection | only the five read operations run; `document`, `image` and `object` vector inputs refused (the server would send them to an outside model); editing refused on read-only connections | — |
| Pinecone | queries never write, on any connection | only the read operations run; a write member hidden in a read refused | — |
| Weaviate | queries never write, on any connection | plain reads only; `nearText` and `hybrid` refused unless every vectorizer is local; `generate`, `ask` and `rerank` refused (they send data to an outside service) | — |
| Chroma | queries never write, on any connection | `get`, `query` and `count` only; query texts only through tobira's embedder, so the server sends nothing out | — |
| Milvus | queries never write; a read-only connection never loads a collection | `query`, `get`, `count` and `search` only; filter functions off the read-only list refused; TEXTEMBEDDING search and the `model` reranker refused (they send data to an outside service) | — |
| pgvector | a similarity search runs in a read-only transaction that is rolled back, on read-write connections too | the PostgreSQL guard | as PostgreSQL |
| DuckDB (plugin) | a `READ ONLY` transaction, rolled back; every instance with external access off and its configuration locked | one statement; no file, URL or SQL-running functions, `COPY`, `ATTACH` or `INSTALL`; only parsed `SELECT` and `EXPLAIN` run | — |

The guards read text and names, so none can see inside code that already lives on the server:
user-defined functions, procedures they call, and remote or external tables. Pair every read-only
connection with a database user, role or key that can only read; that is the real boundary.

## A plugin's guard

A plugin driver that declares the read-only guard is trusted to enforce it. tobira cannot check that
from outside. See [Plugins](/docs/security/plugins/).

When a write is refused and you did not expect it, see
[read-only refusals](/docs/troubleshooting/read-only/).
