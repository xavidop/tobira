# Cross-source queries

Design for running one query over several connections: stage each side with its own native
query, join locally in SQLite, and hand the result to every surface tobira already has (result
tabs, `--format`, export, the AI assistant, the MCP server). Phase two writes the result into one
target collection through the existing pending-changes flow.

## Goal and non-goals

Goal: a user, the assistant or an MCP client can combine rows from two or more configured
connections, whatever their kind (SQL, document, API, vector), without leaving tobira.

Non-goals, deliberately:

- Transparent federation. tobira never parses one SQL statement to discover which tables live
  where, never pushes predicates down and never rewrites per dialect. The user or the model
  splits the work.
- Writes inside a source query. Staging is read-only on every connection, always.
- Several targets in one statement, or atomicity across databases.
- Consistent snapshots across sources. Each source is read independently.

## Statement shape

A federated statement is an ordinary editor buffer or query argument that holds one or more
`@source` directives followed by a final SQLite statement:

```sql
-- @source orders = local-pg/public: select id, user_id, total from orders where created_at > now() - interval '7 days'
-- @source issues = gh: {"collection":"issues","repo":"acme/app","state":"open"}
select o.user_id, count(*) as open_issues, sum(o.total) as spent
from orders o join issues i on i.author_id = o.user_id
group by o.user_id
order by spent desc
```

Rules:

- A directive is a line that starts with `-- @source` or `-- @sink`, at column zero after
  optional whitespace. A buffer with at least one `@source` is federated; any other buffer runs
  exactly as today, so nothing changes for existing users.
- `-- @source <alias> = <connection>[/<namespace>]: <native query>`. The alias becomes a table
  name in the scratch database and matches `[A-Za-z_][A-Za-z0-9_]*`. The connection is a
  configured name; a name with spaces, `/` or `:` goes in double quotes. The namespace is optional
  and becomes `driver.Query.Namespace`.
- A source query continues on the following lines that start with `--` and are not directives,
  with the `-- ` prefix stripped, so a multi-line source stays a comment to every highlighter.
- The final statement is everything that is not a directive block. It is one SQLite statement.
  Statement splitting is skipped for a federated buffer: the whole buffer is one run.
- `-- @sink <connection>[/<namespace>]: <collection>` is phase two, at most one per statement.

Why comments: the buffer stays valid for the SQLite highlighter and for every tool that treats
the text as opaque. Why explicit aliases and per-source native queries: the engine never needs a
parser for any dialect, and a non-SQL source (GitHub, Milvus, Mongo) is first class because its
query is whatever that driver runs today.

## Execution

New package `internal/federate` in the root module. It depends on `driver`, `internal/session`
and nothing database-specific: the scratch engine is reached through the registry's `sqlite`
driver exactly as `internal/history` does, so `make check-deps` stays green.

1. **Parse.** `Parse(text) (Statement, error)` returns the sources, the optional sink and the
   final statement. Errors name the line: duplicate alias, alias that is not an identifier,
   missing `=` or `:`, no final statement, more than one sink, a sink without a source.
2. **Resolve.** Each source connection must be known to the `session.Manager` (TUI, AI, MCP) or
   to the CLI resolver. In the assistant and the MCP server the connections must also be inside
   the turn's allowed set; any other name is refused exactly like an unknown connection.
3. **Stage.** Every source runs concurrently, bounded to four at a time, as
   `driver.Query{Text, Namespace, Limit: maxSourceRows, ReadOnly: true}`. `ReadOnly` is always
   true, regardless of the connection's own setting, so a source query can never write. Rows are
   collected with `driver.Collect`; every source gets a line in the result's `Stats.Warnings`
   ("source orders (local-pg): 10000 rows, more existed" when it was cut). A failing source fails
   the whole run with the source alias in the message, redacted through the manager.
4. **Scratch.** A temporary SQLite file (0600, in `os.TempDir()`, removed when the run ends) is
   opened writable through the registry. The sqlite driver refuses in-memory databases by
   design, and a file also keeps large stagings off the heap. One `CREATE TABLE "<alias>"` per
   source with the source's column names (quoted; a duplicate name gets a `_2`, `_3` suffix) and
   a declared type from `driver.Type`:

   | driver.Type | SQLite column | stored as |
   |---|---|---|
   | int, bool | INTEGER | int64, 0/1 |
   | float | REAL | float64 |
   | decimal | NUMERIC | canonical text, so arithmetic works |
   | string, unknown | TEXT | text, display text |
   | bytes | BLOB | bytes |
   | time | TEXT | RFC 3339 in UTC |
   | json, list, object, vector | TEXT | JSON text, usable with `json_extract` |
   | null | TEXT | NULL |

   A column whose type is null across every row stays TEXT. Rows go in with multi-row
   parameterised `INSERT` statements in batches of 500.
5. **Run.** The final statement runs on the scratch connection with `ReadOnly: true` and the
   caller's row limit. The sqlite driver's own guard refuses anything that is not a read; the
   engine rewrites that refusal as "the final statement of a cross-source query must be a read".
6. **Result.** A `driver.ResultSet` from `driver.NewSliceResultSet`, with the scratch columns
   (no `Source`, so the grid never offers inline editing on it) and a `Stats` whose duration
   covers the whole run and whose warnings list the row count of each source. Every surface that
   accepts a result set today accepts this one unchanged.

`Explain` lists each source with its connection and query, then the final statement. It never
contacts a database: the alias tables' columns are only known once a source has run, so a SQLite
plan would need staging, which explain must not do.

Limits: `maxSourceRows` defaults to 10,000 per source and is configurable up to 1,000,000 with
the `federate.max_source_rows` config key and the `--max-source-rows` flag. The row limit of the
final result is the caller's usual limit.

### History

Each staged query is recorded once on its own connection, with the front end as the source, so
`tobira history` shows what ran where. The final statement is recorded with the connection name
`federated` and the alias list in the query text, so a reader can tie the entries together. The
engine itself writes no history: it has no store. `Run` returns the outcome of every source
(rows, truncation, duration, error) in `Result.Sources`, set even when the run fails after
staging, and each surface records those entries with its own store and source tag, as it records
any other query.

### Security

- Staging is read-only on every connection, including writable ones.
- Staged rows live in a 0600 temporary file that is deleted when the run finishes, and in memory
  only for as long as the run.
- Row data reaches a model only through the existing `send_rows` policy: the final result is a
  query result like any other, and staged rows never go anywhere else.
- The AI and MCP paths narrow the connections a directive may name to the turn's allowed set.
  In the TUI and the CLI the user is the authority, as with any query.
- Plugin drivers work unchanged: values are plain data across the boundary already.

## Surfaces

### CLI

A new command, since `tobira query` needs a connection argument and a federated statement names
its own:

```text
tobira federate [query]     run a cross-source query (-f file, --format, --limit, --timeout, --output, --max-source-rows, --explain)
```

Flags mirror `tobira query`. `--output` streams the final result into a file through
`internal/export`. Exit codes are the ones `tobira query` uses; a source refused as a write exits
with the read-only code. The README's CLI block, the CLI reference page, the agent skill and the
claims scripts (`make check-claims`) all learn the command.

### Terminal UI

- `startRun` parses the buffer first. A federated buffer runs through `internal/federate` with
  the manager, ignores `a.edConn` except as a default, and opens a result tab labelled
  `federated`. No connection has to be selected in the sidebar when every directive names one.
- `:explain` on a federated buffer shows the federate plan described above.
- The editor highlights the final statement as SQLite when the buffer is federated.
- A federated run never participates in an open `:tx`: the run is refused with a message while a
  transaction is open on any of its connections.

### AI assistant

- `assistant.Scope` gains `Conns []string`, the extra connections of the conversation; `Conn`
  stays the primary one, so every existing caller keeps working. `flowScope` mirrors it.
- `schemactx.Build` with several connections builds one section per connection within a shared
  budget, each headed `connection: <name> (dialect, kind)`, and the primary connection keeps the
  largest share.
- The `ask` prompt gets a `federated` branch: how to write `@source` lines in each connection's
  own dialect or JSON syntax, that the final statement is SQLite SQL over the alias tables, that
  the column names of a source query are the columns of its alias table, and that sources are
  capped at `max_source_rows` rows so filters and aggregations belong in the source queries. The
  `fix_error` prompt gets the same context when the failed query is federated. The proposal's
  `Dialect` is `federated`.
- A new read-only tool `federate` takes the statement text and runs it through
  `internal/federate` under the toolset's row policy, cap and consent flow. It is offered in
  every turn whose scope has more than one connection, and `tools.WithConnections` binds the
  turn to all of them.
- The TUI panel gets `:ai scope <name>[,<name>]` to add connections to the conversation; the
  panel title shows them. `tobira ask` accepts a comma-separated list of connections, and
  `--run` runs a federated proposal read-only, as it runs any other.

### MCP server

The `federate` tool is registered whenever more than one connection is exposed. It is read-only,
bounded by `mcp.max_rows`, refuses any connection outside `mcp.expose`, and records its runs in
history with source `mcp` like the other tools. The `execute` policy is untouched.

### Configuration

```yaml
federate:
  max_source_rows: 10000   # rows staged per source, 1 to 1000000
```

### Documentation

A `site/src/pages/docs/cross-source.md` page (shape, limits, type mapping, security), a section
on the AI and MCP pages, the new command in the CLI reference and README, the agent skill, and a
row on the roadmap page when the sink lands.

## Phase two: the sink

`-- @sink staging-mysql/app: user_activity` turns the final result into inserts on that
collection:

- In the TUI the rows become pending inserts of the sink collection, reviewed and committed with
  the existing `:apply`, inside one transaction where the target has `Transactor`. The pending
  set already knows how to insert, so the sink reuses `Mutator.Insert` and the review screen.
- In the CLI `tobira federate --apply` writes after printing the row count, and `--dry-run` only
  prints what would be inserted. Without `--apply` a statement with a sink is refused.
- Refused: a read-only target, a target whose driver has no `Mutator`, a collection that does not
  exist, more than one sink, a sink whose connection is outside the AI or MCP allowed set. The
  assistant may propose a sink, but it only ever lands as pending changes; the `federate` tool
  refuses statements with a sink.
- Result values are the sqlite driver's `driver.Value`s, inserted as an `Object` keyed by the
  result column names. The target driver does its own coercion, as it does for inline edits.

## Milestones

1. **Engine.** `internal/federate`: parse, stage, scratch, run, explain, and the per-source
   outcomes the surfaces record in history. Parsing, staging and type mapping are tested in the
   root module with fake connections and a fake scratch driver; the end-to-end runs are tested
   from `cmd/tobira`, which links the sqlite driver.
2. **CLI and TUI.** `tobira federate`, the editor run path, result tab, explain, highlighting,
   config key, docs and claims scripts.
3. **Assistant.** Multi-connection scope, schema context, prompts, the `federate` tool, panel
   scope command, `tobira ask` with several connections, fake-provider tests.
4. **MCP.** The `federate` tool over the exposed set, docs, agent skill, example client note.
5. **Sink.** Pending inserts in the TUI, `--apply` in the CLI, refusals, docs.

Each milestone merges on its own and leaves every existing path unchanged for a buffer without
directives.

## Status

All five milestones are implemented in the working tree as of 2026-11-05. Every Go file
type-checks under gopls, but no build, test or lint has run yet (the shell was unavailable), so
the first step of the next session is `make all`, then the aifake suites. Deviations from the text
above, all deliberate:

- The engine records no history; each surface records the per-source outcomes in `Result.Sources`
  and the whole statement under `federated` (see History).
- In the TUI the sink collection must be the open browse collection: the rows join the existing
  pending set there, so `:apply`, `:discard`, `i`, `e` and `d` all keep working on them. A sink on
  any other collection is refused with a message saying which collection to open.
- `tobira federate --apply` and `--dry-run` open the connections writable (the sink check needs
  it, and a dry run checks the target exactly as `--apply` would); the sources still
  run read-only per query. Every other `tobira federate` run opens them read-only.
- The assistant's `federate` tool asks row consent once per source connection under
  `send_rows: ask`; one refusal withholds the rows.
- The roadmap page got no row: the sink shipped with the rest, and that page lists only future
  work.
