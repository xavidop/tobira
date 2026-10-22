# tobira Oracle driver

The `oracle` driver lets [tobira](https://github.com/xavidop/tobira) connect to Oracle Database.
It is built into the `tobira` binary and uses the pure-Go `github.com/sijms/go-ora/v2`, so you
need no Oracle Instant Client.

## Connect

| Form | Example |
| --- | --- |
| URL | `oracle://user@localhost:1521/FREEPDB1` |
| URL with TLS | `oracle://user@db.internal/ORCLPDB1?SSL=true` |
| Connect descriptor | `oracle://user@db.internal/?connStr=(DESCRIPTION=...)` |

The path is the service name; the port defaults to 1521. Add `SSL=true` for remote servers. Keep
the password in the connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`. The
connection's `options:` are refused; `TRACE FILE`, `TRACE DIR`, `LOB FETCH=STREAM` and a
`PASSWORD` inside `connStr` are refused too.

## Quick example

```sh
tobira query "oracle://reader@db.internal:1521/ORCLPDB1" "select sysdate from dual" --read-only
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes (`EXPLAIN PLAN`, estimated only) |
| Completion | yes |
| Read-only guard | yes, on Oracle Database 12c to 23ai |

## Read-only

A read-only call must be one `SELECT` or `WITH` query without write words, PL/SQL, database links
or side-effect packages (`DBMS_`, `UTL_` and similar). It runs inside
`SET TRANSACTION READ ONLY`, and tobira checks that it did not end the transaction. Functions
with autonomous transactions are not stopped, so use a `SELECT`-only user for untrusted work.

## Compatibility

Tested with Oracle AI Database 26ai Free 23.26. Read-only access is enabled for Oracle Database
12c to 23ai.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/oracle/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/oracle"
```
