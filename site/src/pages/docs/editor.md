---
layout: ../../layouts/DocsLayout.astro
title: Query editor
---

# Query editor

The query editor runs native statements against a connection. Each run opens a result tab, names
are completed as you type, and every run is kept in the [query history](/docs/history/). A result
can be written to a file with [`:export`](/docs/export/). Statements that change data run here too,
inside a [transaction](/docs/editing/#transactions) if you want one; for row-by-row changes use
[inline editing](/docs/editing/).

| Key | Does |
|---|---|
| `ctrl+e` | Open the editor, focus it, or close it when it has the focus |
| `ctrl+enter` or `F5` | Run the statement under the cursor, or the selection |
| `alt+enter` or `shift+F5` | Run the whole buffer (one statement only) |
| `ctrl+space` | Open [completion](#completion) |
| `↑` / `↓` on the first / last line | Walk the [history](#history) |
| `ctrl+r` | Search the history |
| `[` / `]`, `x` | Previous / next [result tab](#result-tabs), close the tab (grid focused) |
| `ctrl+c` | Cancel a running statement |

`ctrl+e` (or `:editor`) opens a multi-line editor below the grid. It works on the connection of
the open collection, or the one selected in the sidebar, and runs in the open collection's
namespace. Each connection keeps its own buffer for the session. The editor is highlighted for
the connection's dialect:

| Dialect | Drivers |
|---|---|
| PostgreSQL | PostgreSQL, CockroachDB, Amazon Redshift, Greenplum, DuckDB |
| MySQL | MySQL, MariaDB, SingleStore |
| T-SQL | SQL Server |
| CQL | Cassandra |
| JSON | MongoDB, Elasticsearch, OpenSearch, Firestore, Pinecone, Weaviate, Chroma, Qdrant, Milvus |
| Plain SQL | every other driver |

- Type, paste, `enter` (keeps the indent), `tab` (two spaces), arrows, `home`/`end`, `alt+left` and
  `alt+right` by word, `ctrl+home`/`ctrl+end` for the buffer ends. `shift` with a movement key
  selects. The buffer holds at most 1 MiB; a bigger paste is refused.
- `ctrl+enter` runs the statement under the cursor, or the selection. Many terminals send
  `ctrl+enter` only with the kitty keyboard protocol, which tobira asks for; `F5` works everywhere.
  See [how statements are split](#how-statements-are-split).
- `alt+enter` (or `shift+F5`) runs the whole buffer, and refuses a buffer that holds more than one
  statement.
- `ctrl+c` cancels a running statement; with nothing running it quits, as elsewhere.
- `esc` gives the focus back to the grid and leaves the editor open; `ctrl+e` closes it. When the
  completion popup is open, the first `esc` closes only the popup.

## How statements are split

`ctrl+enter` finds the statement under the cursor by splitting the buffer:

- **SQL** is split at `;` outside strings, quoted names, comments and dollar quotes, with each
  dialect's own quoting rules (for example T-SQL `[…]` names and Oracle `q'…'` strings).
- **Oracle PL/SQL**: a block (`BEGIN`, `DECLARE`, `CREATE … PROCEDURE`, `FUNCTION`, `PACKAGE`,
  `TRIGGER` or `TYPE`) keeps its semicolons and runs to a line holding only `/`, or to the end of
  the buffer.
- **JSON queries** are split into their top-level values, never at `;`. So is a Cosmos DB query
  written as a JSON envelope.
- Not recognised: GoogleSQL triple-quoted strings, and `//` comments in CQL and Snowflake.

## Completion

A popup of names opens below the cursor (above it near the bottom), with each name's kind. It opens
by itself after a `.` and once two characters of a name are typed, and `ctrl+space` opens it at any
point. Typing more narrows the list at once (it closes when nothing matches) and asks again after a
short pause; typing never waits for it.

- `↑`/`↓` or `ctrl+p`/`ctrl+n` choose, `tab` or `enter` insert the choice in place of the name being
  typed, and `esc` closes the popup. A name typed inside quotes keeps its quotes. When the popup is
  closed, `enter` and `tab` work as usual. `ctrl+enter`, `F5` and the other run keys close it.
- Names come from the driver's completion (keywords, namespaces, collections and fields, in context)
  plus the names tobira already knows: the namespaces and collections loaded in the sidebar and the
  columns of collections opened in the grid. A driver without completion still gets those. At most
  200 names are shown.
- The popup does not open by itself inside a string or a comment, as far as the highlighter can
  tell (a string opened far above the screen may be missed); `ctrl+space` still works there. Moving
  the cursor off the name, or a lookup that finds nothing, closes the popup.
- Drivers cache catalog names for 60 seconds. `:reload` drops that cache (and tobira's own), and so
  does a successful statement that returns no columns (DDL or DML), so a new table shows up at once.

## History

`↑` on the first line steps back through the queries run on this connection, newest first;
consecutive repeats, and the text you had typed, show once. `↓` on the last line steps forward, and
past the newest query brings back what you had typed. That only works while you are walking:
editing a recalled query keeps the edit and ends the walk, and what you had typed before is gone.

`ctrl+r` searches the history as you type, shown in the editor's last line as
`(reverse-i-search) 'term': <query>` (a multi-line query shows `⏎` for its line breaks). `ctrl+r`
again finds the next older match, `enter` keeps the match in the editor, and `esc` or `ctrl+g` puts
back what was there, as does `ctrl+c` (which does not quit while searching). Any other key keeps the
match and then acts as usual. A failed read shows `search failed`. The search needs an editor at
least two lines tall for its prompt. Both need the
[query history](/docs/history/) file; when it cannot be opened they are off.

## Result tabs

Each run opens a result tab, `N:<first 12 characters>`, next to the browse tab (the open
collection). Up to 9 are kept; a tenth run closes the oldest. `[` and `]` cycle through the tabs and
`x` closes one (not the browse tab). A result shows at most the row limit (`:set limit`), with
`Truncated` in the title when rows were cut; there is no unlimited mode. A statement without
columns shows `N rows affected`. Sort and filter apply to the browse tab only. The status bar shows
the active tab's rows and run time. Click a tab to switch, and click the editor to focus it and
place the cursor.

Runs honour the [read-only](/docs/tui/#read-only) guard: a write on a read-only connection is refused with a
lock message. Every run, `:explain` included, is recorded in the [query history](/docs/history/) with
credentials masked.

## Commands

| Command | Does |
|---|---|
| `:editor` | Open the editor |
| `:explain` | Explain the statement under the cursor (or the selection) into a text tab. Never runs it. Inside `:tx` the transaction explains it. |
| `:set limit <n>` | Row limit of runs, 1 to 100000 (default 1000) |
| `:similar <text>` | On a vector collection, the rows nearest to a text, in a new tab. `S` on a row finds the rows nearest to it. See [vectors](/docs/tui/kind-views/#vectors) |
| `:reload` | Also refreshes the names completion offers |

## Known limitations

Highlighting has one known display limitation. To keep typing fast in large buffers, each edit
re-lexes only the lines on screen plus the 100 lines above them. A string or block comment that
opens more than 100 lines above the visible part can therefore be coloured wrongly until you scroll
up to it. Only the colours are affected: what runs is always the text in the buffer. A pasted tab
becomes two spaces.
