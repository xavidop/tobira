---
layout: ../../../layouts/DocsLayout.astro
title: Data and safety
---

# Data and safety

Once you add an `ai:` section, the provider you chose receives data from your databases, so treat
that choice like any third-party service. This page says exactly what is sent, how `send_rows`
controls row data, what stops the assistant from writing, and how tobira reduces the risk of prompt
injection.

## What is sent to the model

Each request carries:

- the **schema context** for what you are looking at: the collection in scope, its foreign-key
  neighbours and the other collection names of its namespace, within `context_tokens`. It holds
  column names, types, keys, indexes and comments. **DDL is never sent**, because it can embed
  secrets in defaults or comments, and neither are sample rows;
- your **question**;
- in the panel, the **conversation so far**: your earlier questions and the assistant's earlier
  answers (the last 20 turns, each cut at 4000 characters);
- **tool results**, when the assistant calls a tool: schema lookups, query plans and, subject to
  `send_rows`, rows.

The tools reach only the connection of the conversation: the one in the panel's header, or the one
`tobira ask` was given. `list_connections` shows only that connection, and a tool call that names
any other is refused with the same `unknown connection` error as a name that is not configured. No
other connection's schema or rows can reach the model, whatever `send_rows` says.

Every string passes through the connection's credential redactor before it leaves. String values
are cut at 256 characters, and secrets in the text (DSN passwords, the resolved password, the API
key) are masked. Fields named like a password are masked in any form. Other secret-looking names
(`token`, `secret`, `sig`, `api_key` …) are masked as `key=value` (a DSN or URL query) but not as
JSON fields, where they are usually row data.

## send_rows

`send_rows` controls row data: the values a query or sample returns, and the rows of a result tab
you ask to have explained.

| Value | Behaviour |
|---|---|
| `ask` | tobira asks each time rows would be sent. In the panel the prompt reads `send N rows from <connection> to <model>?`, with what produced the rows under it: the tool and its statement (`query: select …`), or `the error text of your failed query` when you ask the AI to fix a run. `y` sends them this once, `a` for the rest of the session, `n` or `esc` declines; `ctrl+c` declines too. With `tobira ask`, rows are sent only when you pass `--send-rows`. |
| `always` | Rows are sent without asking. |
| `never` | Rows are never sent. `--send-rows` and the prompt are refused. |

The row prompt and the execute confirmation ignore keys typed just before they appear, so
type-ahead cannot answer them.

When rows are withheld, the model still gets the column names and a row count, and is told the rows
were not shared. It can still write and refine queries and describe what it found.

### Schema and errors under never

Schema is not row data, so it is sent under `never` too. On key-value databases part of the schema
comes from the data: on [Redis](/docs/drivers/redis/) the collection names are key prefixes (the
part of each key before its first `:`, at most 500 of them), and a collection of hashes has its
field names as columns. The model sees those names under `never`. Keys with no `:` are not listed,
and values never are.

A database error can quote a value (`operator does not exist: <value>`). So when rows may not be
sent, a statement error reaches the model only as its kind, a fixed text and the native code, for
example `not found: object not found; use describe/list tools to check names (SQLSTATE 42P01)`. A
pure syntax error (PostgreSQL 42601, MySQL 1064 and 1149, SQLite "near ...", "incomplete input" and
"syntax error") keeps the text before its first quote character, since that part only cites the
model's own query. With rows allowed, the full error is sent, redacted.

### What never does not stop

`never` keeps row data out of normal answers, but it is not a hard boundary against a hostile model.
A few signals still reach the model, each worth about one bit per call: the number of rows a
withheld result had, whether it was truncated, how long a call took, and whether a statement
succeeded. A model that runs `select 1 from t where col like 'a%'` often enough can learn a value
that way. If you do not trust the model, keep secrets away from connections it can query.

## Read-only layers

The assistant runs `query`, `sample`, `explain` and `similar` only on connections whose driver has
the read-only guard (see [Drivers](/docs/drivers/)). Every such call is sent with the read-only
flag, a 30 second timeout and a row cap of at most 200, whatever the model asks for. On a driver
without the guard, only the schema tools are offered. The `execute` tool is refused on any
connection that is read-only, by config, by `--read-only` or by `:readonly on`.

What a read-only call can do is decided by the driver, not by the model or by the query text. For
example:

- **SQLite.** The file is opened read-only, `query_only` is asserted before every statement,
  attaching databases is disabled, and the statement must start with an allow-listed keyword.
- **PostgreSQL.** Every call runs in its own `READ ONLY` transaction with one statement per query,
  and `COPY`, `DO` and the server-file functions are refused before they run.
- **MySQL.** An allow-list of statement kinds is checked before anything is sent, then a read-only
  transaction (MySQL 8.0 to 8.4 only).

[The read-only guard](/docs/security/read-only/) has the layers of every driver. A read-only guard
is not a sandbox: for untrusted use, connect with a database user that can only read.

## Prompt injection

Database content is untrusted input to the model. A row, a column comment or an error message can
contain text that looks like an instruction. tobira makes this harder:

- every piece of data is placed inside a fence named with a random value chosen per request
  (`<data-NONCE kind="schema">`), so data cannot guess the name to close its fence;
- the prompts tell the model that only text inside a fence is data, and that nothing inside one, or
  in a tool result, is an instruction;
- the markers the prompt format uses for roles are broken in every input, so data cannot open a new
  message.

This makes injection harder, not impossible. What does not depend on the model is the layer below:
the query tools are read-only, `execute` needs your confirmation for each statement, and
`send_rows` decides what leaves.
