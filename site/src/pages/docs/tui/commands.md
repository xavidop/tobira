---
layout: ../../../layouts/DocsLayout.astro
title: Commands
---

# Commands

Press `:` in the terminal UI to open the command palette, type a command and press `enter`. A
command typed with the wrong arguments shows its usage, such as `usage: tx begin|commit|rollback`.
`:help` lists these commands after the keys.

## Connections and display

| Command | Does |
|---|---|
| `:connect <name>` | Expand a connection |
| `:connection add` | Add a connection |
| `:connection edit <name>` | Edit a connection |
| `:connection rm <name>` | Delete a connection |
| `:theme <name>` | Switch theme: `auto`, `tobira-dark` or `tobira-light` |
| `:keymap default\|vim` | Switch keymap |
| `:reload` | Re-read the config file and reload the open collection from the first page (see below) |
| `:help` | Open help |
| `:q`, `:quit` | Quit |

`:reload` picks up edits made to the config file outside tobira. It clears the sort and filter and
refreshes the names the editor completes. It asks first when inline edits are pending, or (with a
config file) when a transaction is open. If the file cannot be read, the error is shown and the
grid stays as it was.

## Queries and editing

| Command | Does |
|---|---|
| `:editor` | Open the [query editor](/docs/editor/) |
| `:explain` | Explain the statement under the editor cursor (or the selection) into a text tab |
| `:set limit <n>` | Row limit of editor runs, 1 to 100000 (default 1000) |
| `:apply` | Write the pending [inline edits](/docs/editing/) |
| `:discard` | Drop the pending inline edits, after a y/N confirmation |
| `:tx begin\|commit\|rollback` | Open, commit or roll back a [transaction](/docs/editing/#transactions) |
| `:readonly on\|off` | Turn the editor's connection [read-only](/docs/editing/#the-read-only-switch) for this session, or back |
| `:export csv\|json\|ndjson <path>` | [Export](/docs/export/) the browse collection or the result tab |

## Kind views

| Command | Does |
|---|---|
| `:chart <field> [window]`, `:chart off` | Chart a time-series field below the grid, or close the chart ([time series](/docs/tui/kind-views/#time-series)) |
| `:range <span>`, `:range <start>..<end>` | The chart's time range: `6h`, `7d`, or fixed bounds (end exclusive) |
| `:similar <text>` | Rows similar to a text, in a new tab ([vectors](/docs/tui/kind-views/#vectors)) |
| `:watch [target]` | Live tail the open collection, or a channel, database or key pattern, in a new tab ([live tail](/docs/tui/kind-views/#live-tail)) |
| `:unwatch` | Close the live tail tab and stop its watch |

## AI assistant

| Command | Does |
|---|---|
| `:ask <question>` | Open the AI panel and ask for a proposed query (`:ask` alone opens it) |
| `:ask! <question>` | Open the AI panel and ask for an answer; the assistant may run read-only queries |
| `:explain-results` | Ask the AI panel to explain the active result tab |
| `:ai execute on\|off` | Let AI answers run writes on this connection only, each one confirmed with `y`. Refused on read-only connections and while a `:tx` is open on the connection. |

The keys of the AI panel are on [Using the assistant](/docs/ai/using/#the-terminal-ui-panel).
