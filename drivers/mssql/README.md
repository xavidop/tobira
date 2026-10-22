# tobira SQL Server driver

The `mssql` driver lets [tobira](https://github.com/xavidop/tobira) connect to Microsoft SQL
Server. It is built into the `tobira` binary and uses `github.com/microsoft/go-mssqldb`.

## Connect

| Form | Example |
| --- | --- |
| URL | `sqlserver://user@host:1433?database=app&encrypt=true` |
| URL with a named instance | `sqlserver://user@host/SQLEXPRESS?database=app` |
| Alias scheme | `mssql://user@host?database=app` |
| ADO (config file, `driver: mssql`) | `server=host;user id=user;database=app` |
| ODBC (config file, `driver: mssql`) | `odbc:server=host;user id=user;database=app` |

In a URL the path is the instance name; the database is `?database=`. Use `encrypt=true` for
remote servers. Keep the password in the connection's `password:` reference or
`TOBIRA_CONN_<NAME>_PASSWORD`. The connection's `options:` are refused; put settings in the DSN.

## Quick example

```sh
tobira query "sqlserver://reader@db.internal:1433?database=app&encrypt=true" "select sysutcdatetime()" --read-only
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (estimated plan only) |
| Completion | yes |
| Read-only guard | yes |

## Read-only

A read-only call must be one `SELECT`, `WITH ... SELECT` or `sp_help` procedure. Data changes,
`SELECT ... INTO`, `EXEC`, `SET`, `OPENROWSET`, `xp_` procedures and locking hints are refused
before anything is sent. The statement then runs in a transaction that is always rolled back.
Functions with side effects outside the database and linked servers are not stopped, so use a
`db_datareader` login for untrusted work.

## Compatibility

Tested with SQL Server 2022. Azure SQL Database, Azure SQL Edge and SQL Server 2025 have not been
tested.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/mssql/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/mssql"
```
