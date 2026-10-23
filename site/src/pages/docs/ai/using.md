---
layout: ../../../layouts/DocsLayout.astro
title: Using the assistant
---

# Using the assistant

You can talk to the assistant in the terminal UI's AI panel, or from the shell with `tobira ask`.
Both use the same tools. This page covers both, plus similarity search on vector databases.

## The terminal UI panel

Press `a` (or run `:ask`) to open the panel on the right of the screen, or over the whole screen on
a terminal narrower than 120 columns. Its header shows the model, the connection and collection in
scope, and the mode.

| Key or command | Action |
|---|---|
| `a` | Open the panel from the grid. While the panel has focus `a` is typed text: press `esc` first, then `a` closes it |
| `ctrl+t` | Switch between **ask** (propose a query, runs nothing) and **answer** (the assistant runs read-only queries) |
| `enter` | Send the question. On an empty input it does nothing |
| `ctrl+o` | Load the latest proposed query into the editor |
| `F5` / `ctrl+enter` | Load the latest proposal into the editor and run it |
| `ctrl+y` | Copy the latest proposal |
| `ctrl+c` | Cancel the turn in progress |
| `esc` | Leave the panel |
| `f` | On a failed run's error tab: ask the AI to fix the query |
| `:ask <question>` | Open the panel and ask for a proposed query |
| `:ask! <question>` | Open the panel and ask for an answer |
| `:explain-results` | Explain the active result tab |
| `:ai execute on\|off` | Let answers run writes on this connection, each confirmed (refused on a read-only connection) |

- **A proposal is only text.** Nothing runs until you load it into the editor and run it, and the
  editor still enforces read-only.
- The proposal keys all take a modifier or a function key, so typing a question never triggers
  them. When the editor holds unsaved text that differs from the proposal, loading it asks
  `replace the editor buffer? y/N` first, because the editor has no undo.
- Each reply ends with a footer: the model, input and output tokens, and how long it took.

### Letting the assistant write

`:ai execute on` gives the assistant the `execute` tool on this connection only. The header then
shows `execute`, and every statement asks
`run this statement on <connection>? it may change data. y/N` before it runs.

- The whole statement is shown in a box under the question. When it is longer than the pane, a line
  under the box reads `… N more lines — ↓/PgDn to read`, and `y` is refused with "read the whole
  statement first" until you have scrolled to its last line.
- `y` is also refused when the pane is too small to show the question up to the connection's name,
  or too narrow to show every character of the statement.
- Execute runs outside the editor's transaction. While a `:tx` is open on the connection it is
  refused with `a transaction is open on <connection>; commit or roll back first`. Commit or roll
  back, then ask again.

## tobira ask

```sh
tobira ask pg "orders per day last week" --collection public.orders
tobira ask pg "how many users signed up today?" --answer --format json
tobira ask pg "the ten newest orders" --run --format table
echo "which tables have no primary key?" | tobira ask pg -
```

- By default `tobira ask` prints a proposed query and its explanation, and runs nothing.
- `--run` then runs the proposal read-only, as `tobira query --read-only` would, and prints the
  result.
- `--answer` lets the assistant run read-only queries and print its answer.

It is never given the `execute` tool. The connection is the first argument (or `--conn`); without
one it uses the only configured connection. Rows are withheld unless you pass `--send-rows`. The
queries an answer runs are recorded in the [history](/docs/history/) with source `ai`. Every flag,
the output formats and the exit codes are in the [`tobira ask` reference](/docs/cli/ask/).

## Similarity search

On a connection whose driver can search vectors (see [Drivers](/docs/drivers/)), the assistant also
gets the `similar` tool. It finds the rows of a collection nearest to a vector, or to a piece of
text, which tobira first turns into a vector with the model set as
[`embedder`](/docs/ai/configuration/#embedder). Without an embedder, searching by text says "text
similarity needs ai.embedder", and searching with a vector still works.

| Argument | Meaning |
|---|---|
| `connection`, `namespace`, `collection` | Where to search, as for `sample`. |
| `vector` or `text` | What to search near. Give exactly one. |
| `k` | How many rows to return: 10 by default, and never more than `max_rows`. |
| `metric` | `l2`, `cosine` or `ip` (inner product). Empty means the collection's own. |
| `filter` | Conditions the rows must all meet, each a `field`, an `op` (`eq`, `ne`, `lt`, `le`, `gt`, `ge`, `contains`, `prefix`, `in`, `is_null` or `not_null`) and a `value`. |

Results come nearest first, with a `_distance` column where smaller is nearer. They follow
`send_rows` like any other rows. A vector in a result is shown as its first 8 numbers followed by
`…`, and the vector a search was given is shown as `<vector dim=N>`. Each search is recorded in the
[history](/docs/history/) with source `ai`, as `similar <collection> k=<k> "<text>"`, or
`similar <collection> k=<k> vector dim=<N>`.
