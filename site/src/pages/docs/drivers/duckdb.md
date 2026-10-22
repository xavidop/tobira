---
layout: ../../../layouts/DocsLayout.astro
title: DuckDB
---

# DuckDB

The DuckDB driver opens [DuckDB](https://duckdb.org) database files. DuckDB needs cgo and the
`tobira` release binary is built without it, so this driver is an **external plugin**,
`tobira-driver-duckdb`. Install it once and tobira offers `duckdb://` connections like any
built-in driver. You can query, browse, edit rows, explain queries and run transactions.

## Install

The plugin is published on the GitHub release of each `drivers/duckdb/v<version>` tag, with one
binary per platform and a checksums file:

| Platform | File |
| --- | --- |
| Linux x86-64 | `tobira-driver-duckdb_<version>_linux_amd64` |
| Linux ARM64 | `tobira-driver-duckdb_<version>_linux_arm64` |
| macOS Intel | `tobira-driver-duckdb_<version>_darwin_amd64` |
| macOS Apple silicon | `tobira-driver-duckdb_<version>_darwin_arm64` |
| Windows x86-64 | `tobira-driver-duckdb_<version>_windows_amd64.exe` |
| Checksums (sha256) | `tobira-driver-duckdb_<version>_checksums.txt` |

Each file is at
`https://github.com/xavidop/tobira/releases/download/drivers/duckdb/v<version>/<file>`. Take the
sha256 of your file from the checksums file and install it; tobira refuses the binary if the
sha256 does not match. For version 1.0.0 on Linux x86-64:

```sh
base=https://github.com/xavidop/tobira/releases/download/drivers/duckdb/v1.0.0
curl -fsSLO "$base/tobira-driver-duckdb_1.0.0_checksums.txt"
sum=$(grep ' tobira-driver-duckdb_1.0.0_linux_amd64$' tobira-driver-duckdb_1.0.0_checksums.txt | cut -d' ' -f1)
tobira driver install "$base/tobira-driver-duckdb_1.0.0_linux_amd64" --sha256 "$sum" --name duckdb
tobira driver info duckdb
```

`--name duckdb` is needed because the release file name carries the version and platform. The
plugin is installed as `tobira-driver-duckdb` in your first `plugins.dirs` entry (by default
`$XDG_CONFIG_HOME/tobira/plugins`).

### Build from source

You need Go and a C toolchain (on Windows, MinGW gcc):

```sh
git clone https://github.com/xavidop/tobira
cd tobira/drivers/duckdb
GOWORK=off go build -o tobira-driver-duckdb .
# Linux:
tobira driver install ./tobira-driver-duckdb --sha256 "$(sha256sum tobira-driver-duckdb | cut -d' ' -f1)"
# macOS:
tobira driver install ./tobira-driver-duckdb --sha256 "$(shasum -a 256 tobira-driver-duckdb | cut -d' ' -f1)"
```

See [Write a driver](/docs/write-a-driver/#external-plugin) and
[Plugin protocol](/docs/plugin-protocol/) for how tobira finds, starts and isolates plugins.

## Quick start

Once the plugin is installed:

```sh
tobira query "duckdb:///data/analytics.duckdb" "select * from events limit 5"
```

```yaml
connections:
  - name: analytics
    dsn: duckdb:///data/analytics.duckdb
    read_only: true
```

## Connecting

| Form | Example |
| --- | --- |
| `duckdb:///<absolute path>` | `duckdb:///data/analytics.duckdb` |
| `duckdb://<relative path>` | `duckdb://./analytics.duckdb` |
| `duckdb:<path>` (config file only) | `duckdb:analytics.duckdb` |
| `duckdb://:memory:` | a private in-memory database (writable connections only) |

- Paths are made absolute and symlinks are followed. Paths holding `#`, `%` or NUL are refused.
- A writable connection creates a missing file. A read-only connection never does: it fails with
  `database file not found`.

| Parameter | Effect |
| --- | --- |
| `access_mode=read_only` | Open the connection read-only. |
| `access_mode=read_write` | The default. |
| `external_access=1` | Writable connections only: allow DuckDB to read files and URLs (see below). |

Any other parameter is refused, because a setting in the DSN could undo the read-only
protections.

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes (namespaces are schemas) |
| Edit rows | yes, by primary key |
| Transactions | yes (snapshot isolation; read-only transactions) |
| Explain | yes (`EXPLAIN`; `EXPLAIN ANALYZE` runs the statement) |
| Completion | yes (DuckDB keywords, schemas, tables, columns) |
| Read-only guard | yes |

## Queries

Write DuckDB SQL, one statement per query. `BEGIN`, `COMMIT` and `ROLLBACK` are refused; use
`:tx begin`, `:tx commit` and `:tx rollback`.

```sh
tobira query analytics "summarize events"
tobira query analytics --explain "select user_id, count(*) from events group by 1"
```

By default DuckDB cannot read files or URLs from SQL: `read_csv`, `FROM 'file.csv'`,
`COPY ... TO`, `ATTACH`, `glob` and httpfs URLs fail on every connection, writable too, and no
`SET` can turn this back on. Extensions are not installed or loaded on their own either. Session
settings such as `search_path` still work.

### External access

A writable connection can opt in with `external_access=1`:

```text
duckdb:///data/analytics.duckdb?external_access=1
```

That restores DuckDB's defaults: `read_csv`, `COPY`, httpfs, extension auto-loading and global
`SET`s such as `threads` or `memory_limit`. On such a connection every read-only query is refused,
because DuckDB itself no longer stops file reads. Open a separate read-only connection for those.
A read-only connection can never opt in.

## Browsing and editing

Pages use `LIMIT` and `OFFSET`, ordered by your sort, then the primary key (or `rowid`). Edit rows
in the grid (`e`, `i`, `d`, then `:apply`); see [Editing rows](/docs/editing/). DuckDB has no
savepoints, so an update or delete first counts the rows its key matches, and changes nothing
unless it is exactly one.

In a transaction, after an error DuckDB aborts the transaction, and only `:tx rollback` works.

## Read-only guard

A connection is read-only when it has `read_only: true` or `access_mode=read_only`, when you pass
`--read-only`, or when you run `:readonly on`. tobira then protects you in four layers:

1. **Allow-list, before anything reaches DuckDB.** One statement led by `SELECT`, `WITH`,
   `VALUES`, `FROM`, `TABLE`, `SHOW`, `DESCRIBE`, `SUMMARIZE`, `PIVOT`, `EXPLAIN` (without
   `ANALYZE`) or a reading `PRAGMA` (`version`, `table_info`, `show_tables`, `database_size` and
   similar). It refuses writes, DDL, `COPY`, `ATTACH`, `USE`, `INSTALL`, `LOAD`, `EXPORT`, `SET`,
   `RESET`, `CALL`, `CHECKPOINT`, `VACUUM`, `PREPARE`, `EXECUTE`, `RETURNING`, `INTO` and
   transaction control anywhere. It refuses functions that read files or URLs or run SQL
   (`read_*`, `parquet_*`, `glob`, `query`, the `sqlite_`, `postgres_`, `mysql_`, `iceberg_` and
   `delta_` scanners, `nextval`). In a table position it refuses a string or a path-like quoted
   name (`FROM 'file.csv'`, `FROM "data/*.parquet"`) and every table function except the
   generators (`range`, `generate_series`, `unnest`) and DuckDB's own catalog functions.
2. **Read-only transaction.** Each statement runs inside `BEGIN TRANSACTION READ ONLY`, which is
   always rolled back.
3. **Statement type.** DuckDB parses the statement first, and only `SELECT` and `EXPLAIN`
   statements run.
4. **The engine.** External access is off and locked, as above, and a read-only connection opens
   the file with `access_mode=READ_ONLY`.

Refused statements make `tobira query` exit with code 4. Row edits are refused too.

To use a refused word as a name, quote it: `select "update" from t`.

**What it cannot stop, and over-refusals.**

- DuckDB allows one configuration per file in a process. If the plugin already holds a writable
  connection to the file, a read-only connection to it shares that one. The other layers still
  apply, but the file is not opened read-only.
- A writable connection cannot open while a read-only one holds its own copy of the file, nor
  while another connection uses a different `external_access`. Close the other connection first.
- Another process can open the file only read-only while one process writes; a second writer
  fails with a conflict.
- The guard refuses some harmless text: `UNPIVOT ... INTO NAME ...` (leave out `INTO`), and a
  string right after `FROM` inside a function, such as `TRIM(BOTH 'x' FROM 'xax')` (write
  `trim('xax', 'x')`) or `extract(year FROM '2020-01-01'::DATE)` (write `DATE '2020-01-01'`).

## Types

| DuckDB | tobira type |
| --- | --- |
| BOOLEAN | boolean |
| TINYINT ... BIGINT, UTINYINT ... UINTEGER | integer |
| UBIGINT, HUGEINT, UHUGEINT, BIGNUM | decimal (exact) |
| DECIMAL(p,s) | decimal (exact, with its scale: `12.50`) |
| FLOAT, DOUBLE | float |
| VARCHAR, ENUM, BIT | text |
| UUID | text |
| INTERVAL | text (`1 year 2 months 3 days 00:00:05.25`) |
| BLOB, GEOMETRY | bytes |
| DATE, TIME, TIMETZ, every TIMESTAMP type | time (TIMESTAMPTZ in UTC) |
| JSON | JSON (object keys come back sorted; numbers past 2^53 lose precision) |
| LIST, ARRAY | list |
| STRUCT | object, members in declaration order |
| MAP | object in map order, each key as text |
| UNION | object with one member, `{tag: value}` |

Large values are cut so they fit the plugin protocol: text and blobs at 512 KiB, lists and maps
at 10,000 items, nesting at 32 levels, and the whole value at about 960 KiB. A cut adds a warning
to the result.

## Limits

- One statement per query.
- No file or URL access from SQL unless you opt in with `external_access=1`.
- Large values are cut, as above.

## Compatibility

Built with DuckDB 1.5.6, linked into the plugin. Tested with that version on real database files,
directly and through tobira.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `unknown driver` for a `duckdb://` DSN | The plugin is not installed. See [Install](#install). |
| `database file not found` | The path is wrong, or the file does not exist and the connection is read-only. |
| `this connection allows external access (external_access=1), so read-only queries are refused` | Open a separate read-only connection without `external_access=1`. |
| `a read-only connection cannot allow external access` | Remove `external_access=1`. |
| `a read-only in-memory database would always be empty` | Use a file, or a writable in-memory connection. |
| `the database file is open in this process with a configuration a read-only connection cannot share` | Close the other connection to the file first. |
| `read-only violation` | The statement is outside the allow-list. Quote names that are refused words. |
