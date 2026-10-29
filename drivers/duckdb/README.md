# tobira DuckDB driver (plugin)

The DuckDB driver lets [tobira](https://github.com/xavidop/tobira) open
[DuckDB](https://duckdb.org) database files. DuckDB needs cgo and the `tobira` release binary is
built without it, so this driver is an external plugin, `tobira-driver-duckdb`, built with
[`github.com/duckdb/duckdb-go/v2`](https://github.com/duckdb/duckdb-go) and tobira's plugin SDK
([`plugin/sdk`](../../plugin/sdk)). Once installed, tobira offers `duckdb://` connections like
any built-in driver.

## Install

Each `drivers/duckdb/v<version>` tag has a GitHub release with one binary per platform,
`tobira-driver-duckdb_<version>_<os>_<arch>` (`linux_amd64`, `linux_arm64`, `darwin_amd64`,
`darwin_arm64`, and `windows_amd64.exe`), and `tobira-driver-duckdb_<version>_checksums.txt`.
Install yours with its sha256 from the checksums file. For version 1.0.0 on Linux x86-64:

```sh
base=https://github.com/xavidop/tobira/releases/download/drivers/duckdb/v1.0.0
curl -fsSLO "$base/tobira-driver-duckdb_1.0.0_checksums.txt"
sum=$(grep ' tobira-driver-duckdb_1.0.0_linux_amd64$' tobira-driver-duckdb_1.0.0_checksums.txt | cut -d' ' -f1)
tobira driver install "$base/tobira-driver-duckdb_1.0.0_linux_amd64" --sha256 "$sum" --name duckdb
tobira driver info duckdb
```

To build from source you need Go and a C toolchain:

```sh
git clone https://github.com/xavidop/tobira
cd tobira/drivers/duckdb
GOWORK=off go build -o tobira-driver-duckdb .
# Linux: sha256sum; macOS: shasum -a 256
tobira driver install ./tobira-driver-duckdb --sha256 "$(sha256sum tobira-driver-duckdb | cut -d' ' -f1)"
```

## Connect

| Form | Example |
| --- | --- |
| Absolute path | `duckdb:///data/analytics.duckdb` |
| Relative path | `duckdb://./analytics.duckdb` |
| Short form (config file only) | `duckdb:analytics.duckdb` |
| In memory (writable only) | `duckdb://:memory:` |

`access_mode=read_only` opens the file read-only. `external_access=1` (writable connections only)
lets SQL read files and URLs; without it, DuckDB's file and network access is off on every
connection.

## Quick example

```sh
tobira query "duckdb:///data/analytics.duckdb" "select * from events limit 5"
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes |
| Completion | yes |
| Read-only guard | yes |

## Read-only

A read-only call passes an allow-list (`SELECT`, `WITH`, `FROM`, `SHOW`, `DESCRIBE`, `SUMMARIZE`,
`EXPLAIN`, reading pragmas, and no file-reading functions), runs in a read-only transaction that
is rolled back, must parse as a `SELECT` or `EXPLAIN`, and runs on an engine with external access
locked off. A read-only connection opens the file with `access_mode=READ_ONLY`, unless the plugin
already holds a writable connection to the same file.

## Compatibility

Built with DuckDB 2.0.0. Tested with that version on real database files, directly and through
tobira.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/duckdb/>

To run the tests:

```sh
cd drivers/duckdb
GOWORK=off go test -race -count=1 ./...
```
