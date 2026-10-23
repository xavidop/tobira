# Changelog

All notable changes to tobira are listed here, newest first. Versions follow [Semantic Versioning](https://semver.org/). Each new release is added at the top, with notes generated from the commit messages.

## [1.4.1](https://github.com/xavidop/tobira/compare/v1.4.0...v1.4.1) (2026-10-23)


### Bug Fixes

* **release:** build the completions outside the workspace so the tree stays clean ([d5571dc](https://github.com/xavidop/tobira/commit/d5571dcd88d615ecb99770be97a860375f0ed5cb))

# [1.4.0](https://github.com/xavidop/tobira/compare/v1.3.0...v1.4.0) (2026-10-23)


### Features

* SSH tunnels, history clear and retention, Linux packages and Scoop ([#46](https://github.com/xavidop/tobira/issues/46)) ([d93c2b4](https://github.com/xavidop/tobira/commit/d93c2b47d7de7c98d7cdbaa4af77ea9d981002f8))

# [1.3.0](https://github.com/xavidop/tobira/compare/v1.2.0...v1.3.0) (2026-10-23)


### Features

* **drivers:** add the Neo4j driver ([#40](https://github.com/xavidop/tobira/issues/40)) ([0ff2cc3](https://github.com/xavidop/tobira/commit/0ff2cc3b42d39250eb584134b59e4e2a4811f709))

# [1.2.0](https://github.com/xavidop/tobira/compare/v1.1.1...v1.2.0) (2026-10-23)


### Features

* **tui:** esc out, tab in, browse tab on open, and the driver's query form for the AI ([#38](https://github.com/xavidop/tobira/issues/38)) ([aefe33b](https://github.com/xavidop/tobira/commit/aefe33b1b3723da2e8ac3a8eb48d46d1f502d7f2))

## [1.1.1](https://github.com/xavidop/tobira/compare/v1.1.0...v1.1.1) (2026-10-23)


### Bug Fixes

* pin load times so golden frames do not depend on machine speed ([#37](https://github.com/xavidop/tobira/issues/37)) ([4fe9d1a](https://github.com/xavidop/tobira/commit/4fe9d1a79a7bc5fc5977f7a2be561c3bdf3d138a)), closes [#34](https://github.com/xavidop/tobira/issues/34) [#35](https://github.com/xavidop/tobira/issues/35)

# [1.1.0](https://github.com/xavidop/tobira/compare/v1.0.1...v1.1.0) (2026-10-22)


### Features

* **tui:** teach the first screen, show tool calls whole, name the connection to the AI ([#34](https://github.com/xavidop/tobira/issues/34)) ([d4b7c38](https://github.com/xavidop/tobira/commit/d4b7c386e9004516e0172f0c69d470d2e1ba1e42)), closes [#33](https://github.com/xavidop/tobira/issues/33) [#23](https://github.com/xavidop/tobira/issues/23) [#32](https://github.com/xavidop/tobira/issues/32) [#23](https://github.com/xavidop/tobira/issues/23) [#24](https://github.com/xavidop/tobira/issues/24) [#25](https://github.com/xavidop/tobira/issues/25) [#26](https://github.com/xavidop/tobira/issues/26) [#27](https://github.com/xavidop/tobira/issues/27) [#28](https://github.com/xavidop/tobira/issues/28) [#29](https://github.com/xavidop/tobira/issues/29) [#30](https://github.com/xavidop/tobira/issues/30) [#31](https://github.com/xavidop/tobira/issues/31) [#32](https://github.com/xavidop/tobira/issues/32)

## [1.0.1](https://github.com/xavidop/tobira/compare/v1.0.0...v1.0.1) (2026-10-22)


### Bug Fixes

* free disk space and allow publishing an existing tag ([#22](https://github.com/xavidop/tobira/issues/22)) ([6661cf9](https://github.com/xavidop/tobira/commit/6661cf9cce30e7253b90552d7e2db1f997ea3889))

# 1.0.0 (2026-10-22)


### Bug Fixes

* apply the Dependabot updates and fix CI ([#21](https://github.com/xavidop/tobira/issues/21)) ([a30e128](https://github.com/xavidop/tobira/commit/a30e128c43f752d9c346e24218399a31a9a91f2b))


### Features

* initial commit ([821be70](https://github.com/xavidop/tobira/commit/821be70add59329a186a5a182d17e0e6f07c6377))

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
