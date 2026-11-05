---
layout: ../../layouts/DocsLayout.astro
title: Cross-source queries
---

# Cross-source queries

A cross-source query combines rows from two or more connections, whatever their kind (SQL,
document, API, vector), without leaving tobira. Each side is staged with its own native query,
read-only, into a scratch SQLite database, and one SQLite statement joins them there. It runs from
the [query editor](/docs/editor/) and from [`tobira federate`](/docs/cli/federate/), and its result
goes wherever any other result goes: a result tab, `--format`, `--output`, [`:export`](/docs/export/).

```sql
-- @source orders = local-pg/public: select id, user_id, total from orders
--   where created_at > now() - interval '7 days'
-- @source issues = gh: {"collection":"issues","repo":"acme/app","state":"open"}
select o.user_id, count(*) as open_issues, sum(o.total) as spent
from orders o join issues i on i.author_id = o.user_id
group by o.user_id
order by spent desc
```

tobira never parses one SQL statement to find out which tables live where, never pushes a filter
down and never translates between dialects. You split the work: each source is a query in that
connection's own language, and the final statement is SQLite SQL over the alias tables. That is
what makes a non-SQL source (GitHub, MongoDB, Milvus) a first-class side of a join: its query is
whatever that driver runs today.

## The statement

A cross-source statement is an ordinary editor buffer or query argument that holds one or more
`@source` directives followed by a final SQLite statement.

- A directive is a line comment that starts with `-- @source`, with any indentation. A buffer with
  at least one `@source` is a cross-source statement and runs as a whole; any other buffer runs
  exactly as before.
- `-- @source <alias> = <connection>[/<namespace>]: <native query>`. The alias becomes a table in
  the scratch database and must be an identifier (letters, digits and `_`, not starting with a
  digit). The connection is a name from the [config file](/docs/configuration/); a name with a
  space, `/` or `:` goes in double quotes, and so does a namespace. The namespace is optional and
  is the query's default namespace (a PostgreSQL schema, a MongoDB database).
- A source query continues on the comment lines that follow, with the `-- ` prefix stripped, so a
  long native query stays a comment to every highlighter.
- The final statement is everything that is not a directive. It is one SQLite statement and must be
  a read; `select`, `with` and `values` are fine, and SQLite's `json_extract`, window functions and
  date functions are all available.
- `-- @sink <connection>[/<namespace>]: <collection>`, at most one, writes the result into that
  collection; see [Writing the result into a collection](#writing-the-result-into-a-collection).

The directives are comments on purpose: the buffer stays valid SQL for the highlighter and for every
tool that treats the text as opaque, and the editor highlights the final statement as SQLite.

## How it runs

1. Every source is checked against the session's connections first; an unknown name, a
   misconfigured entry or a connection with an open `:tx` refuses the run before anything starts.
2. The sources run concurrently, four at a time, each as a read-only query with the staging cap as
   its row limit. Read-only is forced whatever the connection's own setting, so a source query can
   never write. A source that fails stops the others and fails the run, with the alias and the
   connection in the message.
3. The staged rows go into a temporary SQLite file (mode 0600, removed when the run ends), one table
   per alias, in batches of 500 rows. The column names of a source query are the columns of its
   alias table; an empty name becomes `column<n>`, and a repeated name gets a `_2`, `_3` suffix.
4. The final statement runs on the scratch database read-only, with the usual row limit (`:set
   limit`, `--limit`). The result's columns point at no collection, so nothing in it is edited
   inline.

`--explain` on the command line, and `:explain` in the editor, show each source with its connection
and query, then the final statement, and contact no database: the alias tables exist only once the
sources have run.

## Types

Every value keeps as much of its type as SQLite can hold:

| tobira type | SQLite column | stored as |
|---|---|---|
| int, bool | `INTEGER` | the integer; `true` and `false` as 1 and 0 |
| float | `REAL` | the float |
| decimal | `NUMERIC` | the number's text, which SQLite's numeric affinity turns into a number, so arithmetic works |
| string, unknown | `TEXT` | the text, or the display text of a value tobira cannot model |
| bytes | `BLOB` | the bytes |
| time | `TEXT` | RFC 3339 in UTC, which SQLite's date functions read |
| json, list, object, vector | `TEXT` | JSON text, for `json_extract` and `json_each` |
| null | `TEXT` | `NULL` |

A column's declared type comes from the source's column type, or from its first non-null value
when the source does not say (a schemaless store); a column that is null in every row is `TEXT`.

## Limits

- Each source stages at most `federate.max_source_rows` rows (10,000 by default, up to 1,000,000;
  `--max-source-rows` on the command line). A source that was cut is reported in the result's
  warnings as `source <alias> (<connection>): <n> rows, more existed`. Filters and aggregations
  belong in the source queries, where the database does the work; the join belongs in the final
  statement.
- The final result is bounded by the usual row limit.
- Each source is read on its own: there is no consistent snapshot across databases, and no
  transaction. A cross-source query never runs inside `:tx`, and never on a connection with one
  open.
- One statement runs at a time, like any editor run; `ctrl+c` cancels it, and cancellation reaches
  every source.

## Security

- Staging is read-only on every connection, including writable ones, and the final statement is
  read-only too: the scratch database's own guard refuses anything else.
- Staged rows live in a 0600 temporary file that is deleted when the run finishes, and in memory
  only for as long as the run.
- Errors are masked with each connection's settings, as every other error is.
- Driver plugins work unchanged: the engine sees only plain values.

## Writing the result into a collection

```sql
-- @sink staging-mysql/app: user_activity
-- @source orders = local-pg/public: select user_id, count(*) as n, sum(total) as spent from orders group by user_id
-- @source users = crm: select id, name from users
select u.name, o.n, o.spent from orders o join users u on u.id = o.user_id
```

A `-- @sink` directive turns the final result into inserts on one collection, one per row, keyed
by the result's column names. Nothing is written by the query itself: the sink only decides where
the rows go afterwards.

- **In the terminal UI** the sink collection must be open in the browse tab, on its connection.
  When the run finishes, the rows appear there as pending inserts (`+`), exactly as rows you
  insert with `i`: review them, edit a cell, drop one with `d`, then `:apply` writes them, in one
  transaction where the driver has them, or `:discard` drops them. A sink whose collection is not
  the open one, or that cannot be edited (a read-only connection, a driver without edits, no
  primary key), is refused before the run starts.
- **From the command line**, `tobira federate --apply` inserts the rows and `--dry-run` prints
  them; without either flag a statement with a sink is refused. See
  [`tobira federate`](/docs/cli/federate/#writing-the-result-into-a-collection).
- **Refused everywhere:** a read-only target, a target whose driver cannot edit rows, a collection
  that does not exist, more than one sink. The AI assistant may propose a sink, but its `federate`
  tool refuses statements with one: a proposal only ever lands as pending changes you review.
- The target driver coerces the values, as it does for inline edits: a decimal arrives as its
  number, a time as RFC 3339 text, JSON as text.

## With the AI assistant

`:ai scope crm,gh` in the panel, or `tobira ask pg,crm "..."` from the shell, widens a
conversation to several connections. The assistant then gets one schema section per connection,
is told what a query is for each, and may answer with a cross-source statement; in answer mode it
has the `federate` tool, which runs one read-only over the conversation's connections and nothing
else. A proposal loads into the editor like any other and runs into a `federated` tab. See
[Using the assistant](/docs/ai/using/#several-connections-at-once).

## Over MCP

`tobira mcp` offers the `federate` tool whenever more than one connection is exposed. It takes
the statement text, refuses any connection outside `mcp.expose`, runs read-only within
`mcp.max_rows`, and records its runs in the history with source `mcp` like the other tools. See
[Tools and resources](/docs/mcp/tools/#federate).

## Configuration

```yaml
federate:
  max_source_rows: 10000   # rows staged per source, 1 to 1000000
```

See [the federate section](/docs/configuration/#the-federate-section).

## History

Each staged query is recorded on its own connection, with the front end as the source (`cli` or
`tui`), and the whole statement once more under the connection name `federated`, so
`tobira history --conn federated` lists the cross-source runs and `tobira history --conn local-pg`
still shows what ran on that connection. See [Query history](/docs/history/).
