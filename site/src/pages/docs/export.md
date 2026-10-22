---
layout: ../../layouts/DocsLayout.astro
title: Export
---

# Export

tobira writes a result to a csv, json or ndjson file from the command line (`tobira query
--output`) and from the terminal UI (`:export`). Both stream the rows, so the size of the result
does not matter, and both write the file with mode 0600 through a temp file that is renamed over
the target, so a file is either complete or untouched.

## From the command line

```sh
tobira query local-pg "select * from orders" --output ~/orders.csv
tobira query local-pg "select * from orders" --output orders.txt --format ndjson --limit 5000
```

`--output <path>` streams the result into a file in constant memory, so it suits results of any
size. Nothing goes to stdout; one line goes to stderr, such as `wrote 1200 rows to orders.csv`.

| Rule | What happens |
|---|---|
| Format | An explicit `--format` wins. Otherwise the extension decides: `.csv`, `.json`, `.ndjson` or `.jsonl`. Any other extension, and `--format table`, is a usage error. |
| Rows and time | Every row is exported, with no time limit. The `--limit` and `--timeout` defaults do not apply unless you set them. |
| The path | A leading `~` is expanded. The parent directory must exist. A missing directory, or a directory as the target, is a usage error (exit 1) before connecting. |
| The file | Written to a temp file and renamed over the target, with mode 0600. An existing file is replaced, like a shell `>`. A symlink is followed. |
| No result set | A statement such as `UPDATE` writes no file and prints `no result set: 2 rows affected; nothing written to orders.csv`. |
| Errors | Ctrl-C, a timeout or a failed query removes the temp file and exits with the usual code. An existing file keeps its old content. |

### Native csv

For `csv` with no `--limit`, a driver with a native csv export writes the file itself (PostgreSQL
uses `COPY (...) TO STDOUT`). The values are then the server's own text, such as PostgreSQL
booleans as `t`/`f`, and the stderr line counts bytes instead of rows.

- When the driver cannot export the query, tobira writes its own csv instead. PostgreSQL exports
  only a single `SELECT`, `WITH`, `VALUES` or `TABLE` that does not write.
- Set `--limit` to always get tobira's csv.

## In the terminal UI

`:export csv|json|ndjson <path>` writes a file (mode 0600). A leading `~` is your home directory.
The format is always named, so the path's extension does not matter. The file rules of the command
line apply: the parent directory must exist, and a symlink is followed.

- **On the browse tab** it exports the whole collection, page by page, with the grid's filter and
  sort, not only the loaded rows. It is a read, so it works on read-only connections. It reads
  committed data through the connection: pending inline edits are not in the file, and inside
  [`:tx`](/docs/editing/#transactions) it does not see the transaction's uncommitted changes.
  In a collection without a fixed schema (documents, for example) pages can have different
  fields, and no field is dropped. A csv file has one column per field, in the order the fields
  first appear, with an empty cell where a row lacks one. In json and ndjson each row lists its
  own fields, and the json `columns` header lists them all. When a field first appears after the
  first page, a csv or json file is rewritten once at the end to add it, so the export briefly needs
  about twice its size in free disk space.
- **On a result tab** it exports the rows the tab holds and never runs the statement again (that
  could have side effects). When the result was cut at the row limit it says so: "exported N rows
  (the result was cut at the limit; raise :set limit or use tobira query --output)". A text tab
  (rows affected, a plan, an error) cannot be exported.
- An existing file is replaced only after "overwrite <path>? y/N". The file is written to a temp
  file and renamed, so an existing one keeps its content until the export succeeds.
- The status bar shows the progress ("exporting… 12,345 rows"). `ctrl+c` cancels the export, removes
  the temp file and says "export cancelled". One export runs at a time.
- When some values in the file are [previews](#previews), the result says how many: "exported 40
  rows (2 values were previews, not full values) to …".

## Previews

Some drivers browse a shortened preview of a large value rather than the whole value. Redis does:
a string is cut at 4 KiB and a hash, list, set, sorted set or stream at 100 elements. An export of
such a collection writes those previews, and each row's `_truncated` column says whether its value
was cut. tobira counts those rows and warns: on stderr from the command line
(`warning: 2 values were previews, not full values`) and in the result message in the terminal
UI. To get a full value, read it with a query command such as `GET`, `HGETALL` or `LRANGE`.
