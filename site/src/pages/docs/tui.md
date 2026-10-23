---
layout: ../../layouts/DocsLayout.astro
title: Terminal UI
---

# Terminal UI

Run `tobira` in a terminal to browse and query your databases. The terminal UI shows a sidebar of
connections and their collections (tables, for SQL), a data grid with the rows of the one you open,
and a row detail view. From there you can write queries in the [query editor](/docs/editor/),
[edit rows](/docs/editing/) inline, [export](/docs/export/) results and ask the
[AI assistant](/docs/ai/using/#the-terminal-ui-panel).

## Quick start

```sh
tobira                         # every configured connection in the sidebar
tobira connect local-pg        # the same, with local-pg expanded
tobira connect sqlite:./a.db   # a DSN, for this session only; no config file needed
tobira connect local-pg --read-only
tobira --read-only             # every connection in the session read-only
```

Then move with the arrows, press `enter` to open a collection, `f` to filter rows, `s` to sort,
`y` to copy a cell and `q` to quit. `F1` shows every key.

`tobira` starts the terminal UI only when both stdin and stdout are terminals; otherwise it prints
help and exits 0. `tobira connect` needs a terminal and exits 1 without one. See
[`tobira connect`](/docs/cli/connect/) for how it handles a missing or broken
config file.

## In this section

| Page | Covers |
|---|---|
| [Browsing](/docs/tui/browsing/) | The filter grammar, the row detail, copy and mouse |
| [Kind views](/docs/tui/kind-views/) | Documents, key-value stores, wide-column tables, search indexes, time-series charts, vectors and the live tail |
| [Commands](/docs/tui/commands/) | Every `:` command of the command palette |
| [Query editor](/docs/editor/) | Running statements, result tabs, completion and history |
| [Editing rows](/docs/editing/) | Inline edits, `:apply`, transactions and the read-only switch |
| [Keymaps](/docs/keymaps/) | Every key binding, for the default and vim keymaps |

## Layout

- **Sidebar**: connections, each with a status dot, and under each the namespaces and
  collections the driver reports. A connection opens when you expand it. When one fails, select it
  to read the full (redacted) error, and press `enter` to retry. A config entry that could not be
  loaded is listed after the working ones in the failed colour; select it to see what is wrong and
  the keys to edit or delete it (see [broken entries](/docs/connections/#broken-entries)).
- **Grid**: the rows of the open collection, loaded 200 at a time, so large tables stay fast to
  scroll. Until a collection is open, the panel lists the keys to start with: open, add or edit a
  connection, filter the sidebar, the query editor, the palette and help. If a page fails to load, `enter` in the grid tries again. Column widths count characters
  as the terminal draws them, so emoji and combining marks are never split.
- **Status bar**: breadcrumb, row position and load time, `N pending` edits, `TX` while a
  [transaction](/docs/editing/#transactions) is open, the [read-only badge](#read-only),
  connection status, export progress and errors. An error stays until the next key press. On
  the right, the key hints for the focused pane, most useful first: a narrow terminal drops the
  last ones. `F1` or `?` lists every key.
- **Filter bar and command palette** open below the panes when you use them.
- **Result tabs and query editor** share the grid's column when the editor is open: a tab bar on
  top, the grid of the active tab, then the editor at the bottom. On a short terminal the focused
  editor takes the whole column.

Below 60 columns only the focused pane is drawn. `tab` switches between them, and `esc` walks back: from the AI panel or the editor to the grid, and from the grid to the sidebar.

## Keys

The common keys. The [keymaps page](/docs/keymaps/) has every binding for both keymaps.

| Key | Does |
|---|---|
| `tab` / `shift+tab` | Switch pane |
| `esc` | Back: close an overlay, or move the focus from the editor or AI panel to the grid, and from the grid to the sidebar |
| arrows, `pgup`, `pgdown` | Move. The vim keymap adds `hjkl`, `ctrl+d` and `ctrl+u`. |
| `ctrl+home` / `ctrl+end` | Top / bottom. The vim keymap adds `gg` and `G`. |
| `home` / `end` | First / last column |
| `enter` | Expand a connection or open a collection (sidebar), open the row detail (grid) |
| `esc` | Back, close an overlay |
| `/` | Filter the sidebar |
| `f` | Filter the grid rows ([grammar](/docs/tui/browsing/#filter-grammar)) |
| `s` | Sort by the current column: ascending, descending, off |
| `<` / `>` | Narrow or widen the current column |
| `y` / `Y` | Copy the current cell / the row as JSON |
| `:` | [Command palette](/docs/tui/commands/) |
| `F1` or `?` | Help for the current keymap |
| `q` | Quit, asking first about anything that would be lost ([quitting](/docs/editing/#quitting)) |
| `ctrl+c` | Cancel a running `:apply`, run, export or load, then quit |
| `n`, `E`, `D` | Add, edit, delete a connection (sidebar) |
| `ctrl+e` | Open the [query editor](/docs/editor/), focus it, or close it when it has the focus |
| `ctrl+enter` or `F5` | Run the statement under the cursor, or the selection |
| `e` | Edit the current cell ([editing rows](/docs/editing/)) |
| `i` / `d` | Insert a row / mark or unmark the current row for deletion |
| `S` | Find rows similar to this one, on a vector collection ([vectors](/docs/tui/kind-views/#vectors)) |
| `W` | Live tail the open collection ([live tail](#live-tail)) |
| `a` | Open the [AI panel](/docs/ai/using/#the-terminal-ui-panel) |

Choose the keymap with `tui.keymap` in the [config file](/docs/configuration/#the-tui-section) or
`:keymap default|vim`.

## Live tail

`W` or `:watch` opens a live tail of the open collection in a new tab: its change events stream in
as they happen, newest first. It works with drivers that can watch, such as PostgreSQL (`LISTEN`),
MongoDB (change streams) and Redis (keyspace notifications). `p` pauses, `c` clears, and `esc`
closes the tab. The full reference is in [kind views](/docs/tui/kind-views/#live-tail), and
[`tobira watch`](/docs/cli/watch/) streams the same events from a script.

## Read-only

A connection with `read_only: true`, or any connection in a session started with
`tobira --read-only` or `tobira connect --read-only`, shows a lock badge in the status bar (`RO`
when the terminal has no colour). Browsing only reads, and a native `!` filter goes through the same
read-only guard as `tobira query --read-only`, so it cannot write either. Inline editing is refused
on a read-only connection.

`:readonly on|off` switches read-only at runtime; see
[the read-only switch](/docs/editing/#the-read-only-switch).

## Themes

`tui.theme` is `auto` (the default), `tobira-dark` or `tobira-light`. `auto` asks the terminal for
its background colour and picks dark or light. Switch at runtime with
`:theme auto|tobira-dark|tobira-light`. On a terminal with no colour support the status bar uses
ASCII markers.

## Managing connections

`n` in the sidebar adds a connection, `E` edits the selected one and `D` deletes it after you type
its name. Saving writes to the config file and the sidebar updates without a restart. If the file
changed on disk after tobira read it, the edit is refused with
"config file changed on disk; reload with :reload". See [Managing connections](/docs/connections/).

## Demo

![tobira browsing, filtering, running a query and editing a row](/demo.gif)
