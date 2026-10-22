---
layout: ../../layouts/DocsLayout.astro
title: Concepts
---

# Concepts

tobira talks to databases through four ideas: drivers, connections, capabilities and policy. The
[AI assistant](#assistant-and-tools) and the [MCP server](/docs/mcp/) are built on top of them.
Knowing them helps you predict what tobira lets you do on a given connection.

## Driver

A driver teaches tobira one database engine. It reports its name, the DSN schemes it handles, its
kind (such as `sql`, `document` or `keyvalue`) and its native query language, and it knows how to
open a connection. A driver can also be an [external plugin](/docs/write-a-driver/#external-plugin),
a separate program that tobira starts.

## Connection

A connection is what a driver returns when it opens a configuration. Every connection can:

- run a native query (SQL, a MongoDB command, a Redis command, depending on the driver),
- ping and close,
- list namespaces and collections, and describe a collection's schema.

Results stream, so a large result does not have to fit in memory. tobira resolves passwords before
a driver sees them, so a driver never handles your secret references.

## Capability

Anything beyond the core is an **optional capability**. tobira asks each connection what it offers
and adapts. The capabilities are:

| Capability | What it enables |
|---|---|
| browse | Page through a collection in the terminal UI |
| editing | Inline edits, inserts and deletes with `:apply` |
| explain | `--explain` and `:explain` |
| transactions | `:tx begin`, `:tx commit`, `:tx rollback` |
| completion | Name completion in the query editor |
| export | A native export path (for example PostgreSQL's `COPY`) |
| watch | The live tail and `tobira watch` |
| vector search | `S`, `:similar` and the `similar` tool |
| time series | `:chart` and `:range` |
| read-only guard | A hard read-only guard; the AI assistant and MCP server query only such connections |

Every driver browses. The [drivers matrix](/docs/drivers/) lists the capabilities of each driver,
and `tobira driver info <name>` prints them for any driver, plugins included. When a capability is
missing, tobira says so instead of failing: the terminal UI lets you edit rows only when editing is
available, `:tx` only when transactions are, and `:similar` only on a vector search.

## Policy

Policy decides what a connection is *allowed* to do. Capabilities say what is *possible*. You set
policy in four places:

- **Read-only.** `read_only: true` on a connection, `--read-only` on any command, or `:readonly on`
  in the terminal UI. tobira enforces it with the strongest mechanism the database offers, such as
  opening SQLite read-only at the file level or wrapping PostgreSQL work in a read-only
  transaction. A refused write exits with code 4. `:readonly off` cannot lift a read-only setting
  from the config file or the flag. See [the read-only guard](/docs/security/read-only/).
- **The AI assistant.** `ai.send_rows` (`ask`, `always` or `never`) decides whether row data may
  reach the model. The `execute` tool exists only in the terminal UI after `:ai execute on`, and asks
  you to confirm every statement. See [AI assistant](/docs/ai/).
- **MCP.** `mcp.expose` is the set of connections a client can see, `mcp.allow_writes` the subset
  where `execute` exists, `mcp.max_rows` the row cap and `mcp.send_rows` whether rows are sent. See
  [MCP server](/docs/mcp/).
- **Plugins.** `plugins.enabled` and `plugins.dirs` decide which external drivers may start at all.
  See [Configuration](/docs/configuration/#the-plugins-section).

Every built-in driver except SingleStore has the read-only guard. A plugin's read-only guard is the
plugin's own promise, which tobira cannot check from outside.

## Assistant and tools

The [AI assistant](/docs/ai/) uses the same drivers and connections as the rest of tobira. It reads
the schema through namespaces, collections and describe, and it answers by calling a small set of
tools: `list_connections`, `list_namespaces`, `list_collections`, `describe`, `sample`, `query` and
`explain`, plus `similar` on vector databases and `execute` in the terminal UI only.

- The tools reach only the connection of the conversation and refuse any other name.
- The query tools run only on connections with the read-only guard, and always with the read-only
  flag, a timeout and a row cap. The driver's guard stops a write, not the model's good behaviour.
- `ai.send_rows` decides what row data leaves, and `execute` needs your confirmation for each
  statement.
