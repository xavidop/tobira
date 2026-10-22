# tobira SQLite driver

The `sqlite` driver lets [tobira](https://github.com/xavidop/tobira) open SQLite database files.
It is built into the `tobira` binary and uses the pure-Go `modernc.org/sqlite`, so it needs no C
compiler and no system SQLite library.

## Connect

| Form | Example |
| --- | --- |
| `sqlite:<path>` | `sqlite:./app.db` |
| `sqlite3:<path>` | `sqlite3:/var/data/app.db` |
| `sqlite://<path>` | `sqlite:///var/data/app.db` |
| `file:<path>` | `file:app.db` |

Add `?mode=ro` to open the file read-only. Other parameters and in-memory databases are not
supported.

## Quick example

```sh
tobira query sqlite:./app.db "select * from users limit 5"
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (`EXPLAIN QUERY PLAN`) |
| Completion | yes |
| Read-only guard | yes |

## Read-only

A read-only connection opens the file read-only, keeps `PRAGMA query_only` on, and only runs
`SELECT`, `WITH`, `VALUES`, `EXPLAIN` and reading pragmas. Writes, DDL, `ATTACH`, `VACUUM` and
pragmas that change settings are refused before they reach SQLite. A read-only connection never
creates a missing file.

## Compatibility

Built with `modernc.org/sqlite` 1.60. Any SQLite 3 database file works.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/sqlite/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/sqlite"
```
