---
layout: ../../layouts/DocsLayout.astro
title: Editing rows
---

# Editing rows

You can change the rows of the browse tab (the open collection) in place: `e` edits a cell, `i`
inserts a row and `d` marks a row for deletion. Changes stay pending, highlighted in the grid,
until `:apply` writes them. Editing needs a driver that supports edits (see the
[drivers matrix](/docs/drivers/)) and a table with a primary key or an `_id` field.

## Quick start

1. Open a table in the sidebar.
2. Press `e` on a cell, type the new value and press `enter`.
3. Run `:apply` to write the change, or `:discard` to drop it.

## Inline editing

| Key or command | Does |
|---|---|
| `e` | Edit the current cell. `enter` saves, `esc` cancels. |
| `i` | Insert a row, shown above the loaded rows and marked `+`. Fields you leave unset (`default`) are filled by the database. |
| `d` | Mark the current row for deletion (struck through, `-`). `d` again unmarks it. |
| `ctrl+n` | Set the field to NULL, when the field allows it. |
| `:apply` | Write every pending change, in the order you made them. |
| `:discard` | Drop every pending change, after a `y/N` confirmation. |

- **Values are read as the column's type.** For example `42` for an integer, `true` or `no` for a
  boolean, `2026-10-19` or RFC 3339 for a time, and `\x` hex for bytes. A value that does not parse
  is explained under the input, and the editor stays open. Other text is kept as typed.
- **JSON, object and list values** open a multi-line editor with pretty JSON. `enter` adds a
  newline; `ctrl+s` or `alt+enter` saves once the JSON is valid.
- **NULL and empty are different.** An input you empty is the empty string. `ctrl+n` is the only
  way to set NULL. Generated fields cannot be edited.
- **Pending changes** are highlighted and survive paging, sorting and filtering. The status bar
  shows `N pending`.
- **Nothing is lost silently.** With changes pending, opening another collection, changing the
  connection, `:reload` and quitting ask "discard N pending changes? y/N" first.

On a read-only connection `e`, `i` and `d` are refused with a lock message. Result tabs and the row
detail are read-only.

### What `:apply` does

- **With transactions** (see the drivers matrix), the changes run in one transaction. Any error
  rolls it back, nothing is written, and the message names the failing change
  (`update row id=3: ...`).
- **Without transactions**, they run one at a time and stop at the first error. The message says
  how far it got (`applied 3 of 5 — failed at delete id=9: ...`).
- An inserted row that lacks a required field is refused before anything runs
  (`row +2 needs: name, email`).
- `ctrl+c` cancels a running apply. A transaction is rolled back; changes already applied one at a
  time stay applied.
- After a successful apply the grid reloads.
- **Inside an open [transaction](#transactions)**, `:apply` writes through it and does not commit.
  The browse tab shows the changes only after `:tx commit`.

## Transactions

`:tx begin` opens a transaction on the editor's connection, and the status bar shows `TX`. Editor
runs, `:apply` and `:explain` go through it. On a read-only connection the transaction is
read-only, which gives you a consistent snapshot to query.

| Command | Does |
|---|---|
| `:tx begin` | Open a transaction. There is one at a time, and the editor stays on that connection while it is open. |
| `:tx commit` | Commit. A failed commit shows the error, and the transaction is gone. |
| `:tx rollback` | Roll back. |

- The browse tab reads through the connection, not the transaction, so it shows uncommitted
  changes only after the commit.
- After a database error, some databases (PostgreSQL) refuse every statement until you roll back.
  tobira adds "the transaction may be aborted; :tx rollback to continue".
- If a cancelled statement does not stop, the status bar shows `TX!` and only `:tx rollback` is
  allowed.
- Opening a collection on another connection, changing the transaction's connection, `:reload`
  and quitting ask "transaction open on <conn>: [c]ommit · [r]ollback · cancel" first.

## The read-only switch

`:readonly on` makes the editor's connection read-only for the rest of the session; the badge shows
at once. `:readonly off` lifts it, unless the config has `read_only: true` or tobira was started
with `--read-only` ("read-only is enforced by config/flag").

- It is refused while a writable transaction is open on that connection, or while an `:apply` or
  editor run is still writing. The switch never cancels a write for you.
- Pending inline edits stay, but `:apply` refuses them while it is on.

See [Read-only](/docs/tui/#read-only) for the other ways to make a connection read-only.

## Quitting

`q`, `:q` and `ctrl+c` (when nothing is running) quit, after asking about whatever would be lost,
in this order:

1. pending inline edits: "discard N pending changes? y/N";
2. an open transaction: "transaction open on <conn>: [c]ommit · [r]ollback · cancel";
3. a running export: "an export is running; quit and cancel it? y/N";
4. an editor buffer that changed since it last ran: "discard the unsaved query? y/N".

Quitting also cancels a running editor statement or `:apply` without asking. A single statement
is rolled back by the database, so it leaves nothing half done. A cancelled editor run is recorded
in the [query history](/docs/history/) as failed. If the database does not answer a commit,
rollback or cancel, quitting again leaves anyway, says so on stderr, and the connection close rolls
back what was not committed.
