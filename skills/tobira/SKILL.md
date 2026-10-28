---
name: tobira
description: Use when working with tobira, the Go terminal client for many databases (SQL, document, key-value, columnar, time-series, search, wide-column, vector: SQLite, PostgreSQL and pgvector, MySQL, SQL Server, Oracle, Snowflake, BigQuery, ClickHouse, MongoDB, Firestore, Cosmos DB, Redis, DynamoDB, Cassandra, Elasticsearch, InfluxDB, Qdrant, Pinecone, Milvus, Neo4j, GitHub, DuckDB and more), when explaining its driver model, installing it, planning or writing a driver (a Go module or an external plugin), installing a driver plugin with `tobira driver`, using the TUI and its query editor, editing rows, exporting results, running `tobira query`, `tobira watch`, `tobira ask`, `tobira history`, `tobira mcp`, `tobira doctor` or `tobira connections` (including importing from DBeaver, TablePlus or ~/.pgpass and exporting), the AI assistant and its `ai:` config, the MCP server and its `mcp:` config and client setup, configuring connections and secrets, troubleshooting a setup, or answering what the tobira CLI can do. Covers github.com/xavidop/tobira.
---

# tobira: every database, one terminal

tobira (扉, Japanese for *door / gateway*) is a Go CLI and terminal UI that puts one
driver contract in front of many databases, so browsing, querying and explaining work the
same way on all of them. Module: `github.com/xavidop/tobira`.
Docs: https://xavidop.github.io/tobira/docs/

What it has: `tobira` (the terminal UI), `tobira connect`, `tobira connections` (with `export`
and `import`), `tobira query`, `tobira watch`, `tobira ask`, `tobira mcp`, `tobira history`,
`tobira driver`, `tobira doctor` and `tobira version`, with thirty-two drivers (SQLite, PostgreSQL with
pgvector, MySQL, MongoDB, Redis, ClickHouse, Qdrant, InfluxDB, CockroachDB, MariaDB, Redshift,
Greenplum, SingleStore, SQL Server, Cassandra, Oracle, BigQuery, Amazon DynamoDB, libSQL / Turso,
Elasticsearch and OpenSearch, Snowflake, Cloud Firestore, Cloudflare D1, Pinecone, Azure Cosmos DB,
Weaviate, Chroma, Vertica, Milvus, Neo4j, GitHub, plus DuckDB as an external plugin), config and secrets. The TUI
browses, edits rows inline, has a query editor with completion, result tabs and history, runs
transactions, exports to a file, has views per kind of collection (documents, key-value, time
series, vectors) and an AI panel. Drivers can also be external plugins (`tobira-driver-<name>`
programs over gRPC) that `tobira driver` lists and installs. Neo4j is the one graph database
driver and GitHub the one API driver: do not promise others.

## The model in one minute

- A **driver** is a Go module that implements the `driver` contract for one database:
  connect, list, describe, read, explain, with optional mutation and watching.
- Each driver is its **own module** (`drivers/<name>`), so a database SDK never reaches
  anyone who does not use that driver. The root module requires no database SDK.
- The CLI, the terminal UI and the agent-facing tools are written once against the
  contract, not per database.
- A **read-only guard** sits in front of every driver. Connections are writable by default;
  set `read_only: true` on a connection or pass `--read-only` and the guard is a hard
  refusal that the query text cannot lift.
- Available drivers, with their DSN schemes (`tobira driver info <name>` prints them). Capability
  names: browse, explain, editing (`Mutator`), transactions (`Transactor`), completion
  (`Completer`), export (`Exporter`), watch (`Watcher`), similarity search (`VectorSearcher`),
  time-range reads (`TimeSeriesReader`).
  - **SQL:**
    - **SQLite** (`sqlite:`, `sqlite3:`, `file:`), **MySQL** (`mysql://`; 8.0 to 8.4 for
      read-only), **MariaDB** (`mariadb://`, or detected on `mysql://`), **Oracle**
      (`oracle://`), **Vertica** (`vertica://`) and **Snowflake** (`snowflake://`): browse,
      explain, editing, transactions, completion.
    - **libSQL / Turso** (`libsql://`, or `http(s)://`/`ws(s)://` with `driver: libsql`): the
      same set.
    - **SQL Server** (`sqlserver://`, `mssql://`, or ADO/ODBC strings with `driver: mssql`): the
      same set.
    - **PostgreSQL** (`postgres://`, `postgresql://`): adds export (`COPY`), watch (`LISTEN`) and
      pgvector similarity search.
    - **Greenplum** (`greenplum://`): the same as PostgreSQL.
    - **CockroachDB** (`cockroachdb://`, `cockroach://`, or detected on `postgres://`): adds
      export, but no watch or vector search.
    - **Amazon Redshift** (`redshift://`) and **BigQuery** (`bigquery://project[/dataset]`):
      browse, explain and completion; no editing.
    - **SingleStore** (`singlestore://`): browse, explain, editing and completion, but **no
      read-only guard**. Read-only connections and queries are refused.
    - **Cloudflare D1** (`d1://account/database`): browse, explain, editing and completion.
    - **ClickHouse** (`clickhouse://`; columnar): browse, explain, completion and export.
  - **Document:**
    - **MongoDB** (`mongodb://`, `mongodb+srv://`): browse, explain (`executionStats`), editing,
      completion and watch (change streams). Queries are command documents in JSON.
    - **Cloud Firestore** (`firestore://project`): browse, editing, completion and watch. Queries
      are JSON.
    - **Azure Cosmos DB** (`cosmos://account-host?database=…`): browse, editing (ETag match),
      completion, watch (change feed) and vector search. Queries are Cosmos SQL.
  - **Key-value:**
    - **Redis** (`redis://`, `rediss://`): browse (keys with type, TTL and a value preview),
      editing and watch (keyspace notifications). Queries are Redis commands.
    - **Amazon DynamoDB** (`dynamodb://region`): browse, editing, completion and watch (Streams).
      Queries are PartiQL.
  - **Wide-column:** **Cassandra** (`cassandra://`, `scylla://`): browse, editing (conditional
    writes) and completion. Queries are CQL.
  - **Graph:** **Neo4j** (`neo4j://`, `neo4j+s://`, `bolt://`): browse (a label's nodes), explain
    (`EXPLAIN`, `PROFILE`) and a graph neighbourhood reader; read-only is enforced by the server's
    read-mode transactions. Queries are Cypher.
  - **Search:** **Elasticsearch** (`elasticsearch://`) and **OpenSearch** (`opensearch://`):
    browse, explain and editing; vector search (kNN) on Elasticsearch only. Queries are JSON
    request bodies.
  - **Vector:** **Qdrant** (`qdrant://`), **Pinecone** (`pinecone://`), **Weaviate**
    (`weaviate://`), **Chroma** (`chroma://`) and **Milvus** (`milvus://`): browse, similarity
    search and editing. Queries are JSON.
  - **Time series:** **InfluxDB 3** (`influxdb3://`, `influxdb://`): browse measurements and
    time-range reads, in SQL or InfluxQL.
  - **API:** **GitHub** (`github://owner/repo`): a repository's issues, pull requests, comments,
    commits, branches, tags, releases, labels, milestones and workflow runs as collections; browse,
    explain (the requests, never sent), completion and editing (issues, pull requests, comments,
    labels, milestones, releases). Queries are JSON (`list`, `get`, `search`, `repositories`,
    `rest`, `graphql`, `rate_limit`); the token goes in `password:`.
  - **DuckDB** (`duckdb://`, `duckdb:`) is an external plugin, `tobira-driver-duckdb`, built with cgo and
    installed with `tobira driver install`.
  - Every driver but SingleStore declares the read-only guard (`readonly`). Each has its own
    layers; see `SECURITY.md` and each driver's docs page. Several were tested only against an
    emulator or a fake (Snowflake against fakesnow, D1 against a fake REST API, BigQuery,
    Firestore, Cosmos DB, DynamoDB and Pinecone against emulators, and Redshift, Greenplum and
    SingleStore by running the PostgreSQL or MySQL suites in their mode). Say so when it matters.

## Install

- macOS: `brew install xavidop/tap/tobira`. It is a Homebrew cask, so it is macOS only.
- Windows: `scoop bucket add xavidop https://github.com/xavidop/scoop-bucket`, then
  `scoop install tobira`.
- Linux: a `.deb`, `.rpm` or `.apk` (amd64/arm64) from https://github.com/xavidop/tobira/releases,
  installed with `dpkg -i`, `rpm -i` or `apk add --allow-untrusted`. It includes the bash, zsh and
  fish completions. There is no Linux Homebrew formula and no package repository.
- Linux, macOS or Windows: an archive (amd64/arm64, `.tar.gz`, `.zip` on Windows) from the same
  releases page, checked against `checksums.txt`; the archive carries the completions too.
- From a clone: `make build`; the binary lands at `cmd/tobira/tobira`. `go install ...@latest`
  does not work: the CLI module depends on the sibling modules of its checkout.
- Shell completion: `tobira completion bash|zsh|fish|powershell` prints the script; the
  Linux packages install it for bash, zsh and fish.

After installing, `tobira doctor` checks the setup.

## Querying

```sh
tobira query sqlite:./app.db "select * from users" --format json   # table | json | ndjson | csv
tobira query local-pg "select now()" --read-only --limit 100
tobira query local-pg "select * from users where id = 1" --explain
tobira query local-pg "select * from orders" --output orders.csv    # export every row to a file
```

The first argument is a connection name from the config or a DSN. Flags: `-f file` (query from a
file, `-` for stdin), `--format`, `--limit` (default 1000), `--timeout` (default 30s), `--explain`,
`--read-only`, `--output <path>`, `--config`, `--debug`. `--output` streams the result into a csv, json
or ndjson file (format from `--format`, else the extension), exports every row with no timeout unless
`--limit`/`--timeout` is set, prints `wrote N rows to <path>` on stderr and nothing on stdout. Exit codes: 0 ok, 1 usage, 2 connection/auth/unknown driver,
3 other query error, 4 read-only or permission denied. NULL and empty string stay distinct in every
format. One statement per query.

```sh
tobira watch local-pg orders                    # LISTEN on channel orders: one JSON line per NOTIFY
tobira watch mongo shop.orders --max 10         # a change stream on a collection
tobira watch cache 'session:*' --timeout 30s --format table   # Redis keyspace notifications
```

`tobira watch <connection|dsn> <namespace.collection|target>` streams change events (PostgreSQL
and Greenplum `LISTEN`, MongoDB change streams on a replica set, Redis keyspace notifications when
enabled, DynamoDB Streams, Firestore snapshot listeners, the Cosmos DB change feed) as JSON
lines `{"time","op","channel","key","payload"}` (or `--format table`) until Ctrl-C (exit 3,
`canceled`), `--max N` or `--timeout D` (both exit 0). A target with a dot is `namespace.collection`
unless `--channel`; PostgreSQL needs a channel. It works on read-only connections; values are
redacted and escaped. There is no AI or MCP watch tool (a stream does not fit a tool call).

## Terminal UI

- **Starting.** `tobira` in a terminal (stdin and stdout both TTYs) opens the TUI; otherwise it
  prints help. `tobira --read-only` forces every connection read-only. `tobira connect <name|dsn>
  [--read-only]` opens it with one connection expanded (a DSN is for this session only and works
  with no config file; if the file is unreadable it warns and continues with an empty config,
  connection management off).
- **Panes.** Sidebar (connections, collections), grid (rows, 200 per page), row detail, status bar.
- **Keys.** `tab` switch pane, `enter` open (and retry a failed page), `f` filter, `s` sort,
  `y`/`Y` copy cell/row (`y` on NULL copies the empty string, `Y` keeps `null`), `:` command palette
  (`:reload` re-reads the config file), `F1` or `?` help, `q` quit. `ctrl+home`/`ctrl+end` go to the
  top and bottom, `home`/`end` to the first and last column. `tui.keymap: vim` adds hjkl, `gg`,
  `G`, `ctrl+d` and `ctrl+u`. A double-click opens; a single click only selects. All keys:
  https://xavidop.github.io/tobira/docs/keymaps/. Themes: `tui.theme` auto, tobira-dark or
  tobira-light.
- **Filter grammar.** `col op value [and ...]` with `= != < <= > >= ~ ^=` (`~` and `^=` need a
  quoted string) and `is [not] null`, or `!` plus a native filter; there is no `or`.
- **Kind views.** Documents show one-line previews of objects and arrays, columns that grow as pages
  bring new fields, and a dim `—` for a missing field. Key-value collections show `_key`, `type`,
  `ttl` (`∞` for none) and a value preview, with a row detail laid out by type. `:chart <field>
  [window]` charts a numeric field of a time-series collection over `:range 6h` (default the last
  hour). `S` on a row with a vector opens its 20 nearest neighbours in a tab, and `:similar <text>`
  searches by text through `ai.embedder`. A wide-column table marks its partition key `[P]` and
  clustering columns `[C↓]`/`[C↑]`; a search engine's `_score` shows with three decimals. A
  `_truncated` column (a preview the driver cut) is hidden in the grid, shown in the row detail,
  and such a value is never edited inline.
- **Live tail.** `W` or `:watch [target]` opens a tab streaming the open collection's change events
  (or a channel, database or key pattern), newest first, last 1000 kept; `p` pauses/resumes, `c`
  clears, `enter` shows an event in the JSON tree, `esc`, `x` or `:unwatch` closes it. A driver
  without watch support gets "live tail needs a driver with watch support".
- **Query editor.** `ctrl+e` (or `:editor`) opens it below the grid; `ctrl+enter` or `F5` runs the
  statement under the cursor or the selection, `alt+enter` or `shift+F5` the whole buffer (one
  statement only), `ctrl+c` cancels a run, `esc` returns to the grid. Completion opens after `.` or
  two typed characters, or with `ctrl+space` (`tab`/`enter` insert, `esc` closes). `↑` on the first
  line and `↓` on the last walk the connection's history; `ctrl+r` searches it. Each run opens a
  result tab (up to 9; `[`/`]` switch, `x` closes); `:set limit N` (1-100000, default 1000) caps
  rows, `:explain` shows the plan. Runs honour read-only and are recorded in `tobira history` with
  source `tui`.
- **Editing.** Browse tab only; needs a primary key or `_id` and a driver with edits; refused on
  read-only connections. `e` edits a cell (JSON values in a multi-line editor saved with
  `ctrl+s`/`alt+enter`; `ctrl+n` sets NULL), `i` inserts a row, `d` marks a row for deletion. The
  status bar shows `N pending`; `:apply` writes them (one transaction when the driver has one,
  otherwise one at a time, stopping at the first error), `:discard` drops them.
- **Transactions and read-only.** `:tx begin|commit|rollback`: one transaction on the editor's
  connection (status bar `TX`); editor runs, `:explain` and `:apply` go through it (`:apply` does
  not commit). `:readonly on|off` switches read-only for the session (`off` is refused when the
  config or `--read-only` sets it).
- **Export.** `:export csv|json|ndjson <path>` exports the whole browse collection with its filter
  and sort, or the rows a result tab already holds (never re-run).
- **Quitting** asks, in order, about pending edits, an open transaction
  ([c]ommit · [r]ollback · cancel), a running export and an editor buffer changed since it last ran.

Docs: https://xavidop.github.io/tobira/docs/tui/ (also `editor`, `editing` and `export` pages).

## Asking the AI assistant

```sh
tobira ask "orders per day last week" --conn pg --collection public.orders   # proposes a query; runs nothing
tobira ask "how many users signed up today?" --answer --format json           # runs read-only queries itself
echo "tables without a primary key?" | tobira ask - --conn pg                 # question from stdin
```

Flags: `--conn <name|dsn>` (default: the only configured connection), `--collection <ns.name|name>`
(`"a.b"."c.d"` quotes dots), `--answer`, `--send-rows`, `--format text|json`, and `--run`, which
runs the proposed query read-only and prints its result like `tobira query` (with `--limit`,
default 1000, `--timeout`, default 30s, and `--format table|json|ndjson|csv`). Exit 1 usage/config
(also AI not configured), 2 provider auth/unreachable/rate limit, 3 model error and Ctrl-C. It never
has the `execute` tool. Docs: https://xavidop.github.io/tobira/docs/ai/.

Config (nothing is active, and no model is contacted, without an `ai:` section):

```yaml
ai:
  provider: googleai          # googleai (default), vertexai, anthropic, openai, ollama
  model: gemini-3.8-flash     # optional; defaults: googleai/vertexai gemini-3.8-flash,
                              #   anthropic claude-sonnet-5-5, openai gpt-5, ollama llama3.3
  api_key: env:GEMINI_API_KEY # a secret ref, never a literal key; else GEMINI_API_KEY/GOOGLE_API_KEY,
                              #   ANTHROPIC_API_KEY or OPENAI_API_KEY; vertexai uses ADC, ollama no key
  send_rows: ask              # ask (default), always or never
```

Other keys: `base_url` (OpenAI-compatible server, or the ollama address), `project` and `location`
(vertexai), `max_rows` (50, up to 200), `max_tool_calls` (8), `context_tokens` (8000).

`send_rows` governs row data only. The model always gets the schema context (never DDL), the
question, the panel's conversation and tool results. With `ask`, rows are sent only after a yes in
the panel (`y` once, `a` for the session) or `--send-rows` on the CLI; with `never` they never are
(and `--send-rows` is refused). When rows are withheld the model still sees column names and
counts, and driver errors reach it as kind and code only (syntax errors keep their pre-quote
text). `never` is not a cryptographic boundary against an adversarial model: row counts, timing
and success/failure still leak about a bit per call. Query tools run read-only, and only on
connections whose driver declares the `readonly` capability.

In the TUI, `a` opens the AI panel (right side from 120 columns, else full screen): `tab` switches
ask (propose a query) and answer (runs read-only queries), `enter` sends (on an empty input it does
nothing), `ctrl+o` loads the latest proposal into the editor, `F5`/`ctrl+enter` loads and runs it,
`ctrl+y` copies it; `ctrl+c` cancels, `esc` leaves. `f` on a failed run's error tab asks the AI to fix it. Commands: `:ask
<question>`, `:ask! <question>`, `:explain-results` and `:ai execute on|off` (lets answers run
writes on a non-read-only connection, each statement confirmed with `y`). Queries the assistant
runs are in `tobira history` with source `ai`. Prompts can be overridden with files in
`$XDG_CONFIG_HOME/tobira/prompts/` (`ask`, `answer`, `explain_results`, `fix_error`
`.prompt`). `tobira ai dev` is a hidden developer command for the Genkit Dev UI; use test
connections with it.

On a connection whose driver searches vectors (PostgreSQL and Greenplum with pgvector, Qdrant,
Pinecone, Weaviate, Chroma, Milvus, Elasticsearch, Cosmos DB) the assistant
also gets the `similar` tool: `connection`, `collection` (optional `namespace`), exactly one of
`vector` or `text`, `k` (10 by default, at most `max_rows`), `metric` (`l2`, `cosine`, `ip` or
empty for the collection's own) and `filter` (conditions, each `field`, `op`, `value`). Text needs
`ai.embedder` (for example `googleai/text-embedding-005`); without one it says "text similarity
needs ai.embedder" and a vector search still works. Results come nearest first with a `_distance`
column, vectors cut to 8 numbers.

## Query history

Every `tobira query` run and every TUI editor run (including `--explain`, `:explain`, runs the
read-only guard refused, and cancelled runs) is logged to `$XDG_STATE_HOME/tobira/history.db` or `history.path`
(default `~/.local/state/tobira/history.db`, mode 0600, last 10,000 entries or `history.max_entries`)
with passwords and credentials masked. `tobira history [--conn N] [--search TEXT] [--limit N] [--format F]`
lists it, newest first (`--limit 0` means every retained entry). Queries run by the AI assistant's
tools are recorded with source `ai` and MCP calls with source `mcp` (`--mcp`, `--source`). Inline
`:apply` writes and browsing are not recorded. `tobira history clear [--conn N] [--source S] [--mcp]
[--search TEXT] --yes` removes matching entries (every entry without a filter) and compacts the
file; without `--yes` it asks on a terminal and refuses otherwise. Docs:
https://xavidop.github.io/tobira/docs/history/.

## Managing connections

```sh
tobira connections list                       # table on a TTY, JSON otherwise (DSNs redacted)
tobira connections add --name pg --driver postgres --dsn 'postgres://app@host/db' \
  --password-ref env:PG_PASSWORD --read-only --color green --tag prod
tobira connections add            # interactive form (needs a TTY)
tobira connections edit pg        # interactive only
tobira connections test pg        # exit 0 "ok (...)", exit 2 when it cannot connect
tobira connections rm pg --yes
tobira connections rm --line 7 --yes   # an entry by line (no usable name, or a duplicate name)
tobira connections set-password pg    # keep the password in the OS keyring (no echo; --stdin, --no-config, --force)
tobira connections export --format json pg > conns.json   # no passwords; refs kept (yaml by default)
tobira connections import --from dbeaver data-sources.json   # dry-run plan; --apply writes (also pgpass, tableplus, file)
tobira driver list                   # built-in drivers and plugins (--no-start: run no plugin)
tobira driver info memdemo           # Info, capability matrix; a plugin's path and sha256
tobira driver install https://example.com/tobira-driver-memdemo --sha256 <64 hex>   # never runs it
tobira doctor                        # check everything; table on a TTY, JSON otherwise; exit 0 ok, 1 warn, 2 fail
tobira doctor --no-ping --online     # skip the pings; ask the AI provider and each plugin over the network
```

`tobira doctor` reports each check as ok, skip, warn or fail in sections: config, connections
(pinged read-only, 8 at a time, `--timeout` each, 5s by default), secret refs (ok or fail and the
scheme, never the value; remote refs are skipped with `--no-ping`; a scheme no provider in the
build handles fails as `unknown ref scheme`), built-in drivers, plugins (never started without
`--online`), AI (provider, model, key present, embedder; `--online` lists the provider's models),
MCP (exposed names exist, token), history and file permissions (config and history group- or
world-readable, plugins dir writable). A usage error also exits 2. It is the first step for any
"it does not work": read the first `fail` row, then
https://xavidop.github.io/tobira/docs/troubleshooting/.

`tobira connections export [--format yaml|json] [names...]` prints the connections in the config's
own `connections:` shape on stdout: references kept, DSN passwords and secret-looking options
stripped (with a note on stderr). `tobira connections import --from dbeaver|pgpass|tableplus|file
[path]` reads DBeaver's `data-sources.json` (never the encrypted `credentials-config.json`),
TablePlus's `Connections.plist` (macOS; never its keychain), `$PGPASSFILE` or `~/.pgpass` (refused
when group- or world-readable; each line becomes a postgres connection `<host>-<db>`) or an export
file. It is a dry run that prints a plan (add, rename on conflict with `--rename-suffix`, default
`-imported`, skip with a reason) unless `--apply` is given, which writes once, keeping comments. A
password found in the source is offered for the OS keyring (asked per connection on a TTY,
`--keyring-yes` stores without asking, `--no-passwords` ignores them) and the config only ever gets
`password: keyring:<name>`, after the store succeeds.

Also `n`, `E`, `D` in the TUI sidebar. A config entry that fails validation is *broken*, not fatal:
every command warns on stderr (`warning: config <path>: line N: connection "x": <reason>`), `list`
shows it (BROKEN column / `{name,line,broken}` in JSON), the TUI lists it unexpandable, and only a
command that needs it exits 1. Unparseable YAML or a bad `tui:` section is still fatal. Passwords
are references only (`env:NAME`, `file:///path`, `dotenv://path#KEY`, `keyring:NAME`): a hand-written `password:` that
looks like a literal is a load error (URL-form refs such as `op://` load but fail on open).
`connections add` and the TUI form refuse a DSN containing a password; one written into the
config by hand still works (masked everywhere) and `tobira doctor` warns about it. Never write a password into the config
yourself or suggest the user paste one.

## Config and secrets

Config is YAML at `--config`, `$TOBIRA_CONFIG`, `$XDG_CONFIG_HOME/tobira/config.yaml` or
`~/.config/tobira/config.yaml`:

```yaml
connections:
  - name: local-pg
    dsn: postgres://app@localhost:5432/app
    password: env:LOCAL_PG_PASSWORD
    read_only: true
```

A database behind a bastion takes an `ssh:` block on the connection: `host` (or `host:port`),
`user`, `key` (a private key file; else the agent and `~/.ssh/id_*`), `passphrase` and `password`
(secret references), `known_hosts` (default `~/.ssh/known_hosts`; the host key must already be
there, tobira never trusts on first use: `ssh-keyscan -H host >> ~/.ssh/known_hosts`). tobira
forwards a loopback port to the DSN's host and port, so the DSN must be `scheme://host:port/...`
with one host; replica sets, `+srv` and file databases cannot be tunnelled. `~/.ssh/config` is not
read. Docs: https://xavidop.github.io/tobira/docs/connections/ssh/.

Password precedence: `TOBIRA_CONN_<NAME>_PASSWORD`, then the `password:` reference, then the DSN.
References compiled in: `env:`, `file://`, dotenv and `keyring:` (the OS keyring; store a password with
`tobira connections set-password <name>`, which reads it with no echo, or from stdin with `--stdin`). Other
schemes such as `op://` or `aws-sm://` are not compiled in: they load but fail on open. Never put a
real password in the config text.

## Writing a driver (Go module route)

The core contract (`github.com/xavidop/tobira/driver`, no database dependencies):

```go
type Driver interface {
    Info() Info                                   // Name, Kind, Schemes, Dialect, Version, Capabilities
    Open(ctx context.Context, cfg ConnConfig) (Conn, error) // secrets already resolved by the host
}
type Conn interface {
    Query(ctx context.Context, q Query) (ResultSet, error)   // native text, params, limit, read-only flag
    Ping(ctx context.Context) error
    Close() error
    Namespaces(ctx context.Context) ([]Namespace, error)
    Collections(ctx context.Context, ns NamespaceRef) ([]Collection, error)
    Describe(ctx context.Context, ref CollectionRef) (*CollectionSchema, error)
}
```

Optional capabilities are separate interfaces discovered at run time (`driver.As[driver.Browser](conn)`):
`Browser`, `Mutator`, `Explainer`, `Transactor`, `VectorSearcher`, `TimeSeriesReader`,
`GraphReader`, `Completer`, `Exporter` and `Watcher`; each one a driver implements is listed in
`Info().Capabilities`. `ResultSet` streams rows (`Next(ctx)`) with fixed `Columns()` and `Stats()`;
values are a tagged union (null, bool, int, float, decimal, string, bytes, time, JSON, vector,
list, object, unknown). `tobira driver info <name>` prints any driver's capability matrix.

Implement `driver.Driver` and `driver.Conn`, register in `init`, list optional capabilities
(`driver.Browser`, `driver.Explainer`) in `Info().Capabilities`, and prove it with the kit:

```go
func init() { driver.Register(New()) }

func (d myDriver) Info() driver.Info {
    return driver.Info{Name: "mine", Kind: driver.KindOther, Schemes: []string{"mine"}, Dialect: "mine"}
}

func TestConformance(t *testing.T) {
    drivertest.Run(t, drivertest.Config{
        Driver: New(), Open: open(false), OpenReadOnly: open(true), Strict: true,
        Fixture: drivertest.Fixture{ /* Namespace, Collection, SelectAll, Missing, Write */ },
    })
}
```

Errors are `driver.Wrap(driver.ErrNotFound, name, "query", err)`; every call honours `ctx`;
read-only is a hard guard. A complete, passing example is `examples/custom-driver-module`. The
external plugin route is below. Which driver implements which capability is listed above and in
the drivers matrix (https://xavidop.github.io/tobira/docs/drivers/); `tobira driver info <name>`
prints it for one driver. No built-in driver implements `GraphReader`, and its shape may change.
A driver never writes to stderr: it logs through `cfg.Log()` (`ConnConfig.Logger`, the host's
`--debug` log), and marks a cut preview with a `Bool` `_truncated` column after the value.
A driver that enforces `Query.ReadOnly` as a hard guard lists `driver.CapReadOnly` (`"readonly"`) in
`Info().Capabilities`; the conformance kit then probes it with writes, and the AI assistant runs its
query tools only on such connections. A caching `Completer` should also implement the optional
`driver.CatalogInvalidator`.

## Plugins (external drivers)

A plugin is a program named `tobira-driver-<name>` (`^tobira-driver-[a-z0-9][a-z0-9-]{0,62}$`) that
tobira starts on a connection's first use and talks to over gRPC. To everything else it is a driver
called `<name>`; DSNs `<name>://` go to it, and `driver: <name>` on a connection reaches it through
any other scheme. Discovery looks in the plugins directory (`$XDG_CONFIG_HOME/tobira/plugins/`, or
`plugins.dirs`) and then on `PATH`; a built-in driver of the same name wins. `plugins.enabled: false`
turns it off.

```sh
tobira driver install ./tobira-driver-memdemo --sha256 <64 hex>   # --sha256 is required; --name, --force
tobira driver list                    # source builtin or plugin:<path>; shadowed plugins are flagged
tobira driver info memdemo            # starts the plugin: capabilities, path, sha256
tobira query demo "select * from tobira_people"     # with dsn: memdemo:// on connection demo
```

Authoring in Go: implement `driver.Driver` (or the aliases in `github.com/xavidop/tobira/api`), then
`func main() { sdk.Serve(mydriver.New()) }` with `github.com/xavidop/tobira/plugin/sdk` (its own
module). Never write to stdout; log through `cfg.Log()`; `TOBIRA_PLUGIN_LOG_LEVEL` sets the level.
Test it with `drivertest.Run` in Strict mode in process and through `plugin/host`, as
`examples/custom-driver-plugin` (the `memdemo` driver) does. Other languages implement the
`tobira.plugin.v1` Driver service (`plugin/proto/tobira/plugin/v1/driver.proto`, protobuf edition
2023). Docs: https://xavidop.github.io/tobira/docs/write-a-driver/#external-plugin and
https://xavidop.github.io/tobira/docs/plugin-protocol/. After editing the proto, run `make proto`
(`make proto-check` verifies).

Trust: a plugin is code the user runs as themselves, so only suggest installing binaries they trust
and whose sha256 they have checked. It gets an environment allow-list (`PATH`, `HOME`, `TMPDIR`,
`LANG`, `LC_*`, `TZ`, `TOBIRA_PLUGIN_*`), `/dev/null` as stdin, and a connection's password only in
`Open`. World-writable binaries are refused. Limits to state honestly: the host embeds text for
similarity search (a plugin gets no embedder), Windows plugins have no parent-death watch, and only
`name://` DSNs route automatically.

## MCP server

`tobira mcp` serves chosen connections to an MCP client (Claude Code, Claude Desktop, Cursor, VS
Code) over stdio, or Streamable HTTP with `--http`. Nothing is exposed until named; any other
connection name is answered as unknown. Docs: https://xavidop.github.io/tobira/docs/mcp/.

```sh
claude mcp add tobira -- tobira mcp --expose local-pg,cache      # Claude Code
tobira mcp --expose local-pg --http 127.0.0.1:8765               # HTTP: loopback, bearer token
tobira history --mcp                                             # what the clients ran
```

```yaml
mcp:
  expose: [local-pg, cache]   # the only connections a client sees; empty exposes nothing
  allow_writes: [cache]       # a subset of expose; the rest stay read-only
  max_rows: 200               # 1 to 1000
  send_rows: always           # always (default) or never
  token: env:TOBIRA_MCP_TOKEN # HTTP only, a secret reference; else a random token printed once
```

Flags (each replaces its config key): `--expose`, `--allow-writes`, `--max-rows`, `--send-rows`,
`--http host:port`, `--insecure-bind`, `--allowed-host` (repeatable). Tools: `list_connections`,
`list_namespaces`, `list_collections`, `describe`, `sample`, `query`, `explain`, `similar` when an
exposed connection's driver searches vectors (same arguments as the assistant's), and `execute` only
when `allow_writes` is not empty (refused by policy on any other connection and on read-only ones).
Resources: `tobira://<connection>/<namespace>/<collection>/schema`. Exit codes: 1 usage or config
(nothing exposed), 2 stream or listen failure, 4 a non-loopback `--http` without `--insecure-bind`.

Client JSON: Claude Desktop (`claude_desktop_config.json`) and Cursor (`.cursor/mcp.json`) use
`{"mcpServers": {"tobira": {"command": "tobira", "args": ["mcp", "--expose", "local-pg"]}}}`; VS Code
(`.vscode/mcp.json`) uses `{"servers": {"tobira": {"type": "stdio", "command": "tobira", "args": [...]}}}`.
`send_rows` defaults to `always` because nobody is there to answer a prompt; if data must not reach
the model, do not expose the connection. An MCP client is an agent: advise exposing only what it
should see and preferring `read_only` connections. A runnable client is `examples/mcp-client`.

## When helping a user

- Check what exists before promising it. tobira has the TUI (browse, inline editing, a query editor,
  `:tx`, `:readonly`, `:export` and the AI panel), `tobira connect`, `tobira connections` (with
  `set-password`, `export` and `import`), `tobira doctor`, `tobira version`, `tobira query` (with `--output`), `tobira watch`, `tobira ask`, `tobira
  history`, the thirty-two drivers (SQLite, PostgreSQL with pgvector, MySQL, MongoDB, Redis, ClickHouse,
  Qdrant, InfluxDB, CockroachDB, MariaDB, Redshift, Greenplum, SingleStore, SQL Server, Cassandra, Oracle, BigQuery, Amazon DynamoDB, libSQL / Turso, Elasticsearch, OpenSearch, Snowflake, Cloud Firestore, Cloudflare D1, Pinecone, Azure Cosmos DB, Weaviate, Chroma, Vertica, Milvus, Neo4j, GitHub, plus the DuckDB plugin), the kind views, the live tail and similarity search, config and
  `env:`/`file://`/dotenv/`keyring:` secrets, `tobira mcp`, `tobira driver list|info|install` and external driver plugins, the `driver` and `drivertest` packages,
  and the docs site. The Neo4j driver is the one graph database driver.
- When something fails, start with `tobira doctor` (or `--no-ping` offline) and the exit code: 1
  usage/config, 2 connection, 3 query, 4 refused by policy (read-only). The troubleshooting page
  covers the keyring, plugin permissions, read-only refusals, AI keys and MCP tokens.
- Never suggest putting a database SDK in the root module; `make check-deps` rejects it. A
  new database is a new `drivers/<name>` module.
- For a driver contribution, run `drivertest.Run` with `Strict: true` and start from
  `examples/custom-driver-module` (in tree) or `examples/custom-driver-plugin` (external plugin);
  read the contributing guide first.
- Building from source needs only a clone and Go 1.26+.
- Commit scopes are `driver`, `drivertest`, `cli`, `site`, or a plain driver name such as
  `postgres`. Use Conventional Commits.
- Never tell a user the assistant is safe to point at a connection holding secrets "because rows are
  never sent": `send_rows: never` is not a cryptographic boundary. Never put an API key in the
  config; use an `env:` or other reference.
- Never ask a user to paste real credentials or connection strings into an issue or prompt.
