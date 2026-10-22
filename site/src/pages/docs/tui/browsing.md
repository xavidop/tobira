---
layout: ../../../layouts/DocsLayout.astro
title: Browsing
---

# Browsing

Open a collection from the sidebar with `enter` and its rows appear in the grid. This page covers
filtering and sorting rows, the row detail view, copying values and using the mouse.

## Quick start

```text
f            filter:  age >= 18 and name ~ 'Ad'
s            sort by the current column (ascending, descending, off)
enter        open the row detail
y / Y        copy the cell / the row as JSON
```

## Filter grammar

Press `f` and type conditions joined by `and`. Each condition is `column operator value`.

| Operator | Meaning |
|---|---|
| `=` `!=` `<` `<=` `>` `>=` | Compare |
| `~` | Contains (text; the value must be quoted) |
| `^=` | Starts with (text; the value must be quoted) |
| `is null`, `is not null` | NULL tests. `= null` and `!= null` mean the same. |

Values:

- Text in single quotes; a quote inside is doubled: `name = 'O''Brien'`.
- Numbers: `30`, `-4`, `12.50` (kept exactly), `1e3`.
- `true` and `false`.
- `null`, only with `=`, `!=` or `is`.

Column names match exactly, or case-insensitively when that is unambiguous. An unknown column gets
a "did you mean" hint, and a syntax error shows its position. There is no `or`, no parentheses and
no `in`.

```text
age >= 18 and active = true
name ~ 'ito' and email is not null
```

### Native filters

Start the text with `!` to pass the rest straight to the driver as its native filter. For SQL
drivers that is the body of a `WHERE` clause, so it can use `or`. It still goes through the
read-only guard.

```text
! age > 30 or name like 'A%'
```

An empty filter clears it. Changing the sort or the filter reloads from the first page.

## Row detail

`enter` on a row opens it as a tree: one line per column, with objects and arrays (JSON, for
example) that expand with `enter`, `space` or `right` and collapse with `left`.

- `[` collapses every node and `]` expands them all, shallowest levels first. On a huge value it
  stops after about 5,000 lines, and an array longer than 1,000 elements ends with a `… N more`
  line.
- A very large value shows `loading…` while its tree is built. `esc` closes the row detail.
- `y` copies the value under the cursor and `Y` copies the whole row as JSON.
- NULL and the text `NULL` look different, and control characters are shown escaped.

## Copy

`y` and `Y` set the system clipboard with the terminal's OSC 52 escape sequence, so they also work
over SSH if your terminal allows it. A terminal that ignores OSC 52 copies nothing, and tobira
cannot tell. `y` on a NULL cell copies the empty string, while `Y` keeps it as JSON `null`.

## Mouse

- Click a sidebar row to select it, and a grid cell to move there.
- Double-click to expand a sidebar row, open
  a collection or open the row detail.
- The wheel scrolls the pane under the pointer. Clicking a pane also focuses it.

Everything is also reachable from the keyboard.
