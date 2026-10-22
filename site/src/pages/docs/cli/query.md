---
layout: ../../../layouts/DocsLayout.astro
title: tobira query
---

# Run a query

`tobira query` runs one query against a connection and prints the result as a table, JSON, NDJSON
or CSV. Use it in scripts, or for a quick look without opening the terminal UI.

```sh
tobira query sqlite:./demo.db "select * from pets"
tobira query local-pg "select * from orders" --format csv --limit 50
tobira query local-pg -f report.sql --read-only
tobira query local-pg "select * from orders where id = 1" --explain
tobira query local-pg "select * from orders" --output ~/orders.csv
```

## tobira query

```text
tobira query <connection|dsn> [query] [flags]
```

### Arguments

- `<connection|dsn>`: a connection name from the [config file](/docs/configuration/), or a DSN. An
  argument that contains `://` or starts with `sqlite:` or `file:` is a DSN; anything else is looked
  up as a connection name. A name that matches nothing exits 1 and names the closest configured
  connection ("did you mean ...").
- `[query]`: the query text, in the database's own language (SQL for SQL databases). Exactly one
  statement; each driver page says how a second statement is refused. Omit it when you use `-f`.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-f, --file <path>` | | Read the query from a file; `-f -` reads stdin. Not allowed together with a query argument. |
| `--format <name>` | `table` on a terminal, else `json` | `table`, `json`, `ndjson` or `csv`. |
| `--limit <n>` | `1000` | Maximum rows to return; `0` is no limit. When more rows exist the output says it was truncated. |
| `--timeout <duration>` | `30s` | Timeout for the whole run, connecting included; `0` is none. |
| `--explain` | off | Print the query plan instead of running the query. Exits 3 when the driver cannot explain. |
| `--output <path>` | | Write the result to a file instead of stdout. See [Export to a file](#export-to-a-file). Not allowed with `--explain`. |
| `--read-only` | off | Refuse anything that would write, and open the connection read-only (a missing SQLite file is not created). Always on when the connection has `read_only: true`. |
| `--config <path>` | | Config file path (see [Configuration](/docs/configuration/)). |
| `--debug` | off | Write JSON debug logs to the debug log file. |

A negative `--limit` or `--timeout` is a usage error. Ctrl-C cancels the query and exits 3 with
`canceled`. A second Ctrl-C stops tobira at once.

### Export to a file

```sh
tobira query local-pg "select * from orders" --output ~/orders.csv
```

`--output <path>` streams every row into a csv, json or ndjson file. The format comes from
`--format`, or else from the file extension. The file is written atomically with mode 0600, and
only `wrote N rows to <path>` is printed, on stderr (plus a warning when some values were
[previews](/docs/export/#previews)). The rules for the format, the `--limit` and `--timeout`
defaults, the file, PostgreSQL's native csv and errors are on the
[Export](/docs/export/#from-the-command-line) page.

## Output formats

| Format | Shape |
|---|---|
| `table` | Aligned columns by display width, so CJK and emoji line up. Cells are cut with `…` at 60 columns. A footer shows the row count, the duration and whether the result was truncated. |
| `json` | One document: `{"columns": [...], "rows": [{...}], "stats": {...}}`. Rows are objects; repeated column names get `_2`, `_3` suffixes. |
| `ndjson` | One JSON object per row, nothing else. |
| `csv` | RFC 4180 with a header row. |

With `--explain`, `json` prints `{"plan": "...", "format": "text"}` and the other formats print the
plan text.

### NULL and empty strings

Every format keeps `NULL` and the empty string apart:

| Format | NULL | Empty string |
|---|---|---|
| table | `NULL` | blank cell |
| csv | empty, unquoted field | `""` |
| json, ndjson | `null` | `""` |

Decimals keep their scale (`12.50`), and times are RFC 3339 in UTC.

## Exit codes

`tobira query` uses the [shared exit codes](/docs/cli/#exit-codes): 2 when the connection cannot be
opened, 3 for a query error, 4 for a write refused as read-only.
