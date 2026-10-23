---
layout: ../../layouts/DocsLayout.astro
title: Introduction
---

# tobira

`tobira` (扉, "door") is a terminal client for databases, written in Go. You connect, browse the
schema, run native queries and inspect results the same way, whether the database behind the door
is SQLite, PostgreSQL, MongoDB or a vector store. **Every database, one terminal.**

![tobira browsing, filtering, running a query and editing a row](/demo.gif)

## Quick start

```sh
brew install xavidop/tap/tobira        # macOS; Windows: scoop install tobira; Linux: a .deb, .rpm or .apk
tobira query sqlite:./demo.db "select sqlite_version()"
tobira connect sqlite:./demo.db        # open the terminal UI
```

The [Quick start](/docs/quickstart/) walks through a first session.

## What you can do

| Task | Where to read |
|---|---|
| Browse tables and collections, filter, sort and copy rows in a terminal UI | [Terminal UI](/docs/tui/) |
| Write queries with completion, result tabs and history | [Query editor](/docs/editor/) |
| Edit rows inline, inside a transaction if you want | [Editing rows](/docs/editing/) |
| Run one query from a script, as a table, JSON, NDJSON or CSV | [`tobira query`](/docs/cli/query/) |
| Export a result to csv, json or ndjson | [Export](/docs/export/) |
| Keep passwords out of the config file, in env vars, files or the OS keyring | [Passwords and secrets](/docs/secrets/) |
| Import connections from DBeaver, TablePlus or `~/.pgpass` | [Importing connections](/docs/connections/import/) |
| Ask questions about your data in plain language | [AI assistant](/docs/ai/) |
| Let Claude Code, Cursor or another MCP client query chosen connections | [MCP server](/docs/mcp/) |
| Stream change events from PostgreSQL, MongoDB, Redis and other databases | [Live tail](/docs/tui/#live-tail) and [`tobira watch`](/docs/cli/watch/) |
| Check your setup | [`tobira doctor`](/docs/cli/doctor/) |
| Add a database that is not listed | [Write a driver](/docs/write-a-driver/) |

## Databases

tobira ships with thirty-one drivers: SQLite, PostgreSQL (with pgvector), MySQL, MariaDB, SingleStore,
Amazon Redshift, CockroachDB, Greenplum, SQL Server, Oracle, Snowflake, BigQuery, Vertica,
libSQL / Turso, Cloudflare D1, MongoDB, Cloud Firestore, Azure Cosmos DB, Amazon DynamoDB, Redis,
Cassandra, ClickHouse, InfluxDB, Elasticsearch, OpenSearch, Qdrant, Pinecone, Weaviate, Chroma,
Milvus and Neo4j. DuckDB is an external plugin that you [install separately](/docs/drivers/duckdb/#install).
The [drivers page](/docs/drivers/) shows what each one supports.

## AI ready, CI ready

- Ask questions in plain language with [`tobira ask`](/docs/ai/) or the AI panel, using Gemini,
  OpenAI, Anthropic or a local Ollama model.
- [`tobira mcp`](/docs/mcp/) lets Claude Code, Cursor and other agents query the connections you
  choose.
- [`tobira query`](/docs/cli/query/) prints JSON, NDJSON or CSV with clear exit codes, and
  [`tobira doctor`](/docs/cli/doctor/) checks your setup, so tobira fits in scripts and CI.

## Safe by default

- Mark a connection `read_only: true`, or pass `--read-only`, and tobira refuses anything that
  would write. The guard uses the strongest mechanism each database offers. See
  [the read-only guard](/docs/security/read-only/).
- Passwords are references such as `env:PG_PASSWORD` or `keyring:prod`, never values in the config
  file.
- The AI assistant runs read-only queries. It writes only in the terminal UI, after `:ai execute on`,
  and you confirm each statement. It asks before it sends row data to a model.
- The MCP server exposes nothing until you name a connection.

[Security](/docs/security/) covers what tobira protects and what it does not.

## Use it as a library

The stable `api` package lets another Go program use tobira's drivers without the terminal UI or a
config file. See [`examples/embedding`](https://github.com/xavidop/tobira/tree/main/examples/embedding).

## Next steps

- [Install](/docs/install/) tobira.
- Follow the [Quick start](/docs/quickstart/).
- Read [Concepts](/docs/concepts/) for drivers, connections, capabilities and policy.
- Stuck? Start with [Troubleshooting](/docs/troubleshooting/).
