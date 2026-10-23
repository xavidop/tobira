---
layout: ../../../layouts/DocsLayout.astro
title: Drivers
---

# Drivers

tobira ships with thirty built-in drivers, plus DuckDB as an external plugin. Elasticsearch and
OpenSearch share one row but count as a driver each; pgvector is part of the PostgreSQL driver. Each
driver's page has its DSN format, examples, capabilities and read-only behaviour.

DuckDB is not part of the binary: [install the plugin](/docs/drivers/duckdb/#install) from its
GitHub release, or build it from source.

## Driver matrix

| Driver | Kind | Browse | Explain | Vector search | Time series | Other capabilities | Tested against |
|---|---|---|---|---|---|---|---|
| [SQLite](/docs/drivers/sqlite/) | SQL | yes | yes (no `ANALYZE`) | no | no | editing, transactions, completion, read-only guard | real database |
| [PostgreSQL](/docs/drivers/postgres/) | SQL | yes | yes (`ANALYZE` rolled back) | yes ([pgvector](/docs/drivers/pgvector/) extension) | no | editing, transactions, completion, export, watch, read-only guard | real server |
| [MySQL](/docs/drivers/mysql/) | SQL | yes | yes (no `ANALYZE`) | no | no | editing, transactions, completion, read-only guard | real server |
| [MariaDB](/docs/drivers/mariadb/) | SQL | yes | yes (no `ANALYZE`) | no | no | editing, transactions, completion, read-only guard | real server |
| [SingleStore](/docs/drivers/singlestore/) | SQL | yes | yes (no `ANALYZE`) | no | no | editing, completion | MySQL and MariaDB (SingleStore mode) |
| [CockroachDB](/docs/drivers/cockroachdb/) | SQL | yes | yes (`ANALYZE` rolled back) | no | no | editing, transactions, completion, export, read-only guard | real server |
| [Amazon Redshift](/docs/drivers/redshift/) | SQL | yes | yes (text plan, no `ANALYZE`) | no | no | completion, read-only guard | PostgreSQL 17 (Redshift mode) |
| [Greenplum](/docs/drivers/greenplum/) | SQL | yes | yes (`ANALYZE` rolled back) | yes ([pgvector](/docs/drivers/pgvector/) extension) | no | editing, transactions, completion, export, watch, read-only guard | PostgreSQL 17 (Greenplum mode) |
| [SQL Server](/docs/drivers/mssql/) | SQL | yes | yes (estimated plan, never runs) | no | no | editing, transactions, completion, read-only guard | real server |
| [Oracle](/docs/drivers/oracle/) | SQL | yes | yes (estimated plan, never runs) | no | no | editing, transactions, completion, read-only guard | real server |
| [libSQL / Turso](/docs/drivers/libsql/) | SQL | yes | yes (no `ANALYZE`) | no | no | editing, transactions, completion, read-only guard | real server (sqld) |
| [Cloudflare D1](/docs/drivers/d1/) | SQL | yes | yes (no `ANALYZE`) | no | no | editing, completion, read-only guard | local fake of the D1 API |
| [ClickHouse](/docs/drivers/clickhouse/) | Columnar SQL | yes | yes (no `ANALYZE`) | no | no | completion, export, read-only guard | real server |
| [BigQuery](/docs/drivers/bigquery/) | Columnar SQL | yes | yes (dry run: bytes and schema; no `ANALYZE`) | no | no | completion, read-only guard | BigQuery emulator |
| [Snowflake](/docs/drivers/snowflake/) | Columnar SQL | yes | yes (`EXPLAIN USING TEXT`, never runs; no `ANALYZE`) | no | no | editing, transactions, completion, read-only guard | local fake (fakesnow) |
| [Vertica](/docs/drivers/vertica/) | Columnar SQL | yes | yes (plan only, no `ANALYZE`) | no | no | editing, transactions, completion, read-only guard | real server |
| [MongoDB](/docs/drivers/mongodb/) | Document | yes | yes (`executionStats`, never applies a write) | no | no | editing, completion, watch, read-only guard | real server |
| [Cloud Firestore](/docs/drivers/firestore/) | Document | yes | no | no | no | editing, completion, watch (snapshot listeners), read-only guard | Firestore emulator |
| [Azure Cosmos DB](/docs/drivers/cosmos/) | Document | yes (continuation tokens) | no | yes (`VectorDistance`; L2, cosine, inner product; text with an embedder) | no | editing (ETag match), completion, watch (change feed), read-only guard | Cosmos DB emulator |
| [Redis](/docs/drivers/redis/) | Key-value | yes | no | no | no | editing, watch, read-only guard | real server |
| [Amazon DynamoDB](/docs/drivers/dynamodb/) | Key-value | yes | no | no | no | editing, completion, watch (Streams), read-only guard | DynamoDB Local |
| [Cassandra](/docs/drivers/cassandra/) | Wide-column | yes (paging state; tobira-side sort up to 10,000 rows) | no | no | no | editing (conditional writes), completion, read-only guard | real server |
| [Elasticsearch / OpenSearch](/docs/drivers/elasticsearch/) (2 drivers) | Search | yes (`search_after` on a point in time) | yes (`_validate/query`, never runs; `ANALYZE` runs the search with profiling) | Elasticsearch: yes (kNN on `dense_vector`, exact `script_score` for other metrics; text with an embedder); OpenSearch: no | no | editing, read-only guard | real servers |
| [InfluxDB](/docs/drivers/influxdb/) | Time series | yes | no | no | yes (windowed ranges, SQL or InfluxQL) | read-only guard | real server |
| [Qdrant](/docs/drivers/qdrant/) | Vector | yes | no | yes (per-vector metric, text with an embedder) | no | editing, read-only guard | real server |
| [pgvector](/docs/drivers/pgvector/) (part of PostgreSQL) | Vector | yes | yes | yes (L2, cosine, inner product; text with an embedder) | no | read-only guard | real server |
| [Pinecone](/docs/drivers/pinecone/) | Vector | yes (list order; tobira-side filters, sort up to 10,000 records) | no | yes (index metric, text with an embedder) | no | editing, read-only guard | Pinecone Local |
| [Weaviate](/docs/drivers/weaviate/) | Vector | yes (cursor API; offset pages when filtered or sorted) | no | yes (per-vector metric, text with an embedder) | no | editing, read-only guard | real server |
| [Chroma](/docs/drivers/chroma/) | Vector | yes (Chroma's order; tobira-side sort up to 10,000 records; server-side filters) | no | yes (collection's space, text with an embedder) | no | editing, read-only guard | real server |
| [Milvus](/docs/drivers/milvus/) | Vector | yes (primary-key order; sorted by tobira up to 10,000 entities) | no | yes (index metric; text via BM25/TEXTEMBEDDING functions or an embedder) | no | editing, read-only guard | real server (2.6 standalone) |
| [DuckDB](/docs/drivers/duckdb/) (plugin) | SQL | yes | yes (`EXPLAIN`; `ANALYZE` runs it, on read-only paths only for reads) | no | no | editing, transactions, completion, read-only guard | real database |

Every driver can run queries, list namespaces and collections, and describe a collection.

- **Vector search**: similarity search with the terminal UI's `S` and `:similar`, and the
  `similar` tool of the AI assistant and the MCP server.
- **Time series**: time-range reads with the terminal UI's `:chart` and `:range`.
- **watch**: change events in the terminal UI's [live tail](/docs/tui/#live-tail) (`W`, `:watch`)
  and in `tobira watch`.
- **read-only guard**: a statement run read-only cannot write, whatever its text says. The AI
  assistant and the MCP server run queries only on connections whose driver has it. SingleStore has
  no read-only mode, so tobira refuses read-only connections to it; use a `SELECT`-only user there. See
  [the read-only guard](/docs/security/read-only/).

`tobira driver info <name>` prints the same capabilities for any driver, plugins included.

## What each driver was tested against

The "Tested against" column says what each driver has been tested with. "Real server" means the
database itself. Where the real service cannot run locally, the driver is tested against the
vendor's emulator or a local fake. Redshift and Greenplum are tested by running the PostgreSQL
suite in Redshift or Greenplum mode, and SingleStore by running the MySQL and MariaDB suites in
SingleStore mode. Each driver's page lists the known differences.

No driver has been tested yet against the cloud service it names (for example BigQuery, Snowflake
or Cloudflare D1 in the cloud), or against a real Redshift cluster, Greenplum cluster or
SingleStore server. Emulators can behave differently from the real service; see
[emulators and fakes](/docs/troubleshooting/drivers/#emulators-and-fakes).

## Compatible databases and services

Many databases speak a protocol that a built-in driver already handles, so they need no driver of
their own: connect with the driver named here and its DSN format. tobira's CI tests the servers in
the matrix above, not these services, and what a service leaves out of the protocol tobira cannot
do either.

| Database or service | Use the driver | Notes |
|---|---|---|
| Neon, Supabase, AlloyDB, Amazon Aurora PostgreSQL, Amazon RDS for PostgreSQL, Google Cloud SQL for PostgreSQL, Azure Database for PostgreSQL | [PostgreSQL](/docs/drivers/postgres/) | the PostgreSQL wire protocol |
| YugabyteDB, TimescaleDB, Citus | [PostgreSQL](/docs/drivers/postgres/) | PostgreSQL-compatible; their own SQL functions work as queries |
| Amazon Aurora MySQL, Amazon RDS for MySQL, Google Cloud SQL for MySQL, Azure Database for MySQL, PlanetScale, TiDB, Vitess | [MySQL](/docs/drivers/mysql/) | the MySQL wire protocol |
| ScyllaDB | [Cassandra](/docs/drivers/cassandra/) | CQL; see the Cassandra page |
| Valkey, KeyDB, Dragonfly, Amazon ElastiCache, Google Memorystore, Azure Cache for Redis | [Redis](/docs/drivers/redis/) | the RESP protocol |
| Amazon DocumentDB, Azure Cosmos DB for MongoDB, FerretDB | [MongoDB](/docs/drivers/mongodb/) | the MongoDB wire protocol; each implements a subset of the commands |
| Turso | [libSQL / Turso](/docs/drivers/libsql/) | covered on the libSQL page |
| OpenSearch | [Elasticsearch / OpenSearch](/docs/drivers/elasticsearch/) | its own driver, `opensearch` |
| ClickHouse Cloud | [ClickHouse](/docs/drivers/clickhouse/) | |
| Zilliz Cloud | [Milvus](/docs/drivers/milvus/) | managed Milvus |
| Qdrant Cloud, Weaviate Cloud, Chroma Cloud | the driver of the same name | the hosted service's URL and API key in the DSN |

## A database that is not listed

See [Write a driver](/docs/write-a-driver/). A driver can be an external plugin, so it does not have
to wait for a tobira release.
