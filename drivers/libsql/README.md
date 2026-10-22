# tobira libSQL driver

The `libsql` driver lets [tobira](https://github.com/xavidop/tobira) connect to
[libSQL](https://github.com/tursodatabase/libsql) servers: `sqld` (libsql-server) and
[Turso](https://turso.tech). It is built into the `tobira` binary and talks to the server over
HTTP. The SQL is SQLite's.

## Connect

| Form | Example |
| --- | --- |
| `libsql://host[:port]` | `libsql://my-db-my-org.turso.io` (HTTPS) |
| `libsql://host:port?tls=0` | `libsql://localhost:8080?tls=0` (plain HTTP) |
| `https://`, `http://` (with `driver: libsql`) | `https://sqld.internal:8443` |
| `wss://`, `ws://` (with `driver: libsql`) | the same as `https://` and `http://` |

Put the Turso token or sqld JWT in the connection's `password:` (a secret reference) or
`TOBIRA_CONN_<NAME>_PASSWORD`; a token in a query parameter is refused. Plain HTTP to a host
other than your machine also needs `allow_plaintext=1`, and is always refused for `*.turso.io`.

## Quick example

```sh
tobira query "libsql://my-db-my-org.turso.io" "select * from users limit 5" --read-only
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

A read-only call must be one `SELECT`, `WITH`, `VALUES`, `EXPLAIN` or reading pragma, with no
`INSERT`, `UPDATE`, `DELETE` or `REPLACE INTO` inside it. It then runs between `BEGIN` and
`ROLLBACK` on the server. For a hard guarantee, use a read-only token
(`turso db tokens create --read-only`).

## Compatibility

Tested with sqld 0.24 with JWT authentication. Not yet tested against Turso itself, which serves
the same protocol.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/libsql/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/libsql"
```
