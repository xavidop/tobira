---
layout: ../../../layouts/DocsLayout.astro
title: Tools and resources
---

# Tools and resources

An MCP client reaches your exposed connections through tools and schema resources. This page lists
them, and explains how `send_rows`, writes and the call history work.

## Tools

| Tool | What it does |
|---|---|
| `list_connections` | The exposed connections and their drivers. |
| `list_namespaces` | Schemas or databases of a connection. |
| `list_collections` | Tables or collections of a namespace. |
| `describe` | Columns, keys and indexes of a collection (never DDL). |
| `sample` | A few rows of a collection. |
| `query` | Run one read-only statement. A statement that writes is refused. |
| `explain` | The query plan of a statement. |
| `similar` | The rows of a collection nearest to a vector or a text. Offered only when an exposed connection's driver can search vectors. |
| `execute` | Run a write. Offered only when `allow_writes` is not empty. |

`query`, `sample`, `explain` and `similar` always run with the read-only guard, a timeout and the
row cap. Results keep large integers exact.

### similar

`similar` takes a `connection`, a `collection` (and optionally a `namespace`), and exactly one of
`vector` (a list of numbers) or `text`.

- Text is turned into a vector with the `ai.embedder` model from the config file. Without one, a
  text search fails with "text similarity needs ai.embedder"; a vector search still works.
- `k` is how many rows to return: 10 by default, at most `max_rows`.
- `metric` is `l2`, `cosine` or `ip`, or empty for the collection's own.
- `filter` is a list of `conditions`, each a `field`, an `op` and a `value`, that the rows must all
  meet.

Rows come nearest first, with a `_distance` column. A vector in a result is cut to its first 8
numbers and `…`. The arguments are the same as for the
[AI assistant](/docs/ai/using/#similarity-search).

There is no watch tool. A change stream runs until it is stopped, and a tool call returns one
answer. Use the terminal UI's [live tail](/docs/tui/#live-tail) or
[`tobira watch`](/docs/cli/watch/) instead.

## Resources

Every collection of an exposed connection can be read as a resource, so a client can attach a
schema without a tool call:

```text
tobira://<connection>/<namespace>/<collection>/schema
```

The content is the same JSON as the `describe` tool, as `application/json`. A connection that is
not exposed reads as "not found", the same as one that does not exist. Any read that names nothing
readable gets MCP's resource-not-found error (`-32002`), with the same text for all of them.

## Rows: send_rows

`send_rows: always` (the default) returns row data in tool results. `never` returns the column
names and a count only, and reduces driver errors to their kind and code.

The default is `always`, unlike the AI assistant's `ask`, because an MCP client runs unattended and
there is nobody to answer a prompt. If the data must not reach the model, do not expose the
connection.

- Schema is still sent under `never`. On [Redis](/docs/drivers/redis/), schema comes from the data:
  collection names are key prefixes (the part of each key before its first `:`, at most 500 of
  them), and a collection of hashes has its field names as columns. Keys with no `:` are not listed.
- `never` is not a hard boundary against a hostile model: row counts, timing and success or failure
  still leak.

## Writes

Reads are the default, and `execute` does not exist until you allow it. With
`allow_writes: [cache]`:

- `execute` is offered, and runs only on `cache`.
- On any other exposed connection, and on a `read_only: true` connection, it answers
  `refused by policy: ...`.
- `query` still refuses writes, even on `cache`.

There is no per-call confirmation: the client decides. Allow writes only on a connection where an
unattended agent writing is acceptable, such as a scratch or test database.

## History

Every tool call and resource read is written to the local [query history](/docs/history/) with
source `mcp` and the client's `name/version`, from its `initialize` request.

- Refused calls are recorded as `refused by policy: <statement>`, and a call to a tool the server
  does not offer as `unknown tool <name>`.
- A `similar` search is recorded as `similar <collection> k=<k> "<text>"`, or
  `similar <collection> k=<k> vector dim=<N>`.
- Credentials are masked like any other history entry.

```sh
tobira history --mcp
tobira history --mcp --format json
```
