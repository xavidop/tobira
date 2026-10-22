# tobira BigQuery driver

The `bigquery` driver lets [tobira](https://github.com/xavidop/tobira) connect to Google
BigQuery. It is built into the `tobira` binary and uses the official Go client,
`cloud.google.com/go/bigquery`. You write GoogleSQL.

## Connect

| Form | Example |
| --- | --- |
| Project | `bigquery://my-project` |
| Project and default dataset | `bigquery://my-project/sales` |
| With options | `bigquery://my-project/sales?location=EU&max_bytes_billed=10737418240` |
| Emulator | `bigquery://test-project/sales?emulator=localhost:9050` |

The connection's `password:` must be a service-account JSON key (as a secret reference); without
one, Application Default Credentials are used. Credentials in the DSN are refused.
`max_bytes_billed` caps every job (1 GiB by default, `0` for no cap).

## Quick example

```sh
tobira query "bigquery://my-project/sales?location=EU" "select count(*) from orders" --read-only
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Explain | yes (dry run) |
| Completion | yes |
| Edit rows, transactions, native export | no |
| Read-only guard | yes |

## Read-only

A read-only call must be one `SELECT` or `WITH` query with no write, script or DDL words. BigQuery
then dry-runs it, and the statement type must be `SELECT`. Every job carries the byte cap. Remote
and ML functions are not stopped; restrict them with IAM.

## Compatibility

Tested with the goccy BigQuery emulator 0.8.1. Not yet tested against Google Cloud.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/bigquery/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/bigquery"
```
