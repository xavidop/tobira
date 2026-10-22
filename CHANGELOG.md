# Changelog

All notable changes to tobira are listed here, newest first. Versions follow [Semantic Versioning](https://semver.org/). Each new release is added at the top, with notes generated from the commit messages.

## 1.0.0 highlights

What tobira 1.0.0, the first public release, includes: one CLI and terminal UI for SQL, document,
key-value, wide-column, search, time-series and vector databases.

### Added

#### Terminal UI
- `tobira` opens the terminal UI, and `tobira connect <connection|dsn>` opens it on one connection
  or DSN, with no config file needed.
- A sidebar of connections and collections, a data grid with paging, sort and a filter bar, and a
  row detail tree for JSON values.
- Mouse support, themes (`auto`, `tobira-dark`, `tobira-light`), a default and a vim keymap, a
  command palette and clipboard copy.
- A query editor with highlighting for each dialect, completion, history search (`ctrl+r`),
  result tabs, `:explain` and cancellation.
- Inline editing: `e` edits a cell, `i` inserts a row, `d` deletes one, and `:apply` writes the
  pending changes, in one transaction where the database has them.
- `:tx begin|commit|rollback`, `:readonly on|off` and `:export csv|json|ndjson <path>`.
- Views for each kind of database: document previews, key-value types and TTLs, `:chart` for time
  series, partition and clustering key badges for wide-column tables, search scores, and
  similarity search with `S` or `:similar <text>`.
- A live tail (`W` or `:watch`) that streams a collection's change events into a tab.
- Quitting never drops pending edits, an open transaction or an unrun query without asking.

#### Command line
- `tobira query` runs one query and prints a table, JSON, NDJSON or CSV, with `--limit`,
  `--timeout`, `--explain`, `--read-only` and `--output` to stream a result of any size into a
  file. Exit codes tell a bad connection (2) from a failed query (3) from a refused write (4).
- `tobira watch` streams change events as JSON lines.
- `tobira connections list|add|edit|rm|test|set-password|export|import` manages connections.
  Import reads DBeaver, TablePlus, `~/.pgpass` or an export file, shows a dry-run plan and writes
  only with `--apply`.
- `tobira history` lists every query tobira, the AI assistant and MCP clients ran.
- `tobira doctor` checks the config, connections, secret references, drivers, plugins, the AI
  provider, the MCP policy and file permissions, and exits 0, 1 or 2 for ok, warnings or failures.
- `tobira driver list|info|install` shows drivers and installs plugins.
- `tobira version` prints the version, commit and build date.

#### Drivers
- Thirty built-in drivers:
  - SQL: SQLite, PostgreSQL (with pgvector), MySQL, MariaDB, SingleStore, CockroachDB, Amazon
    Redshift, Greenplum, SQL Server, Oracle, libSQL / Turso and Cloudflare D1.
  - Columnar SQL: ClickHouse, BigQuery, Snowflake and Vertica.
  - Document: MongoDB, Cloud Firestore and Azure Cosmos DB.
  - Key-value and wide-column: Redis, Amazon DynamoDB and Cassandra (also `scylla://`).
  - Search: Elasticsearch and OpenSearch.
  - Time series: InfluxDB 3.
  - Vector: Qdrant, Pinecone, Weaviate, Chroma and Milvus.
- DuckDB as an external plugin, `tobira-driver-duckdb`, published on the GitHub release of each
  `drivers/duckdb/v*` tag, or built from source.
- A read-only guard for every driver except SingleStore, which has no read-only transaction, so
  tobira refuses read-only connections to it.
- Change events for PostgreSQL and Greenplum (`LISTEN`), MongoDB change streams, Redis keyspace
  notifications, DynamoDB Streams, Firestore snapshot listeners and the Cosmos DB change feed.

#### AI assistant
- An AI panel in the TUI (`a`) and `tobira ask` on the command line. The assistant proposes a
  query, answers by running read-only queries, explains a result and fixes a failed query.
- Providers: Gemini (Google AI or Vertex AI), Claude, OpenAI or any OpenAI-compatible server, and
  Ollama.
- `ai.send_rows` (`ask`, `always` or `never`) controls whether row data reaches the model.
- Writes only in the TUI, after `:ai execute on`, with a confirmation for each statement.
- A `similar` tool on connections that support vector search, with text search through
  `ai.embedder`.
- Prompts you can override with files in `$XDG_CONFIG_HOME/tobira/prompts/`.

#### MCP server
- `tobira mcp` serves the connections you name to an MCP client (Claude Code, Claude Desktop,
  Cursor, VS Code) over stdio, or over HTTP with `--http`.
- Read-only tools (`list_connections`, `list_namespaces`, `list_collections`, `describe`,
  `sample`, `query`, `explain`, `similar`), schema resources, and `execute` only on connections in
  `mcp.allow_writes`.
- Every MCP call is recorded in the history, and `tobira history --mcp` shows them.

#### Plugins and the Go API
- External driver plugins: a `tobira-driver-<name>` program, in any language, that tobira starts on
  first use and talks to over gRPC. The Go SDK (`plugin/sdk`) serves a driver with `sdk.Serve`.
- The `driver` contract, the `drivertest` conformance kit and the stable `api` package for
  programs that embed tobira's driver layer.
- Examples: a custom driver module, a custom driver plugin, embedding tobira in a Go program, and a
  small MCP client.

#### Install
- Release archives for Linux, macOS and Windows (amd64 and arm64) with checksums, a Homebrew cask
  for macOS (`brew install xavidop/tap/tobira`) and `go install`.
- An agent skill for AI coding agents (`npx skills add xavidop/tobira`).

### Security
- Passwords are references (`env:`, `file://`, dotenv, `keyring:`), never stored in the config
  file. `tobira connections add` and the TUI form refuse a DSN that carries a password; one
  written into the config by hand still works, is masked everywhere, and `tobira doctor` warns
  about it.
- Passwords and credentials are masked in errors, logs, the TUI, the history and `tobira doctor`.
- Read-only connections are enforced by the database session where the database allows it, not
  by text checks alone. A statement the checks cannot recognise as a read is refused.
- The HTTP and gRPC drivers, Cassandra, Vertica and ClickHouse refuse to send a credential in
  plain text to a host that is not this machine unless you add `allow_plaintext=1`. Managed cloud
  services always use TLS, and no HTTP driver follows a redirect with a credential.
- Control, bidirectional and zero-width characters in values, names and errors print as escapes.
- The MCP server over HTTP needs a bearer token, checks the Host and Origin headers, and listens on
  loopback unless you pass `--insecure-bind`.
- `tobira driver install` requires a `--sha256` checksum. Plugins get an allow-list of environment
  variables, no stdin, and a connection's password only when the connection opens.
- History and export files are written with mode 0600.

### Compatibility
- Redshift, Greenplum and SingleStore were tested on PostgreSQL or MySQL, not on their own servers.
- Snowflake, Cloudflare D1, BigQuery, Cloud Firestore, Azure Cosmos DB, Amazon DynamoDB and
  Pinecone were tested against emulators or fakes, not the cloud services.
- The MySQL-family read-only guard needs MySQL 8.0 to 8.4 or MariaDB 11.1 to 11.x.
