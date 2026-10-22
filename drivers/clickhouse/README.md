# tobira ClickHouse driver

The `clickhouse` driver lets [tobira](https://github.com/xavidop/tobira) connect to ClickHouse.
It is built into the `tobira` binary and uses the official Go client,
`github.com/ClickHouse/clickhouse-go`, over the native protocol or HTTP.

## Connect

| Form | Example |
| --- | --- |
| Native | `clickhouse://user@localhost:9000/default` |
| Native with TLS | `clickhouse://user@ch.internal/analytics?secure=true` (port 9440) |
| HTTPS | `clickhouse://user@ch.example.com:8443/analytics?protocol=http&secure=true` |
| Several hosts | `clickhouse://user@ch1,ch2:9441/analytics?secure=true` |

Keep the password in the connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`.
Settings go in the DSN; the connection's `options:` are refused. Without `secure=true`, plain HTTP
to a host other than your machine needs `allow_plaintext=1`, and so does a password over the native
protocol. Both are always refused for ClickHouse Cloud.

## Quick example

```sh
tobira query "clickhouse://reader@ch.internal:9440/analytics?secure=true" "select count() from events" --read-only
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Explain | yes (`EXPLAIN indexes = 1`) |
| Completion | yes |
| Native csv export | yes (`CSVWithNames`) |
| Edit rows, transactions | no |
| Read-only guard | yes |

## Read-only

A read-only call passes an allow-list (`SELECT`, `WITH`, `SHOW`, `DESCRIBE`, `EXPLAIN`, `EXISTS`)
and is sent with `readonly=1`, so ClickHouse refuses writes, schema changes, setting changes and
table functions such as `url`, `s3` and `remote`. For users whose profile is already read-only,
tobira checks table functions itself. Tables with external engines are not stopped; grants decide
who can read them.

## Compatibility

Tested with ClickHouse 25.8, over the native protocol and over HTTP.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/clickhouse/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/clickhouse"
```
