# tobira Azure Cosmos DB driver

The Azure Cosmos DB for NoSQL driver for
[tobira](https://github.com/xavidop/tobira), built on the Azure SDK for Go
(`azcosmos`). It registers the `cosmos` driver for the `cosmos://` scheme and
is compiled into the `tobira` binary. An account with the API for MongoDB
works through the `mongodb` driver instead.

## Connect

```text
cosmos://myaccount.documents.azure.com?database=shop
cosmos://myaccount.documents.azure.com:443?database=shop
cosmos://localhost:8081?database=shop&tls=false
```

The host is the account endpoint and `database` the default database. The
account key goes in the connection's `password:` reference. Parameters:
`database`, `tls`, `allow_plaintext` and `key` (discouraged; use the
password). `tls=false` to a host that is not this machine also needs
`allow_plaintext=1`, and Azure's own endpoints are always TLS.

## Quick example

A query is one Cosmos SQL `SELECT`; the container is the name after `FROM`.

```sh
tobira query shop "SELECT * FROM orders o WHERE o.status = 'paid'" --read-only
```

To read one logical partition or pass named parameters, send a JSON envelope:

```json
{"container":"orders","query":"SELECT * FROM c WHERE c.total > @min","partitionKey":"acme","parameters":{"@min":10}}
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, containers, describe | yes; describe samples 100 items |
| Browse | yes; filters become `WHERE`, sorting `ORDER BY` |
| Editing | yes; insert, update (with an ETag match) and delete of one item |
| Vector search | yes; `VectorDistance` on a container with a vector policy |
| Completion | yes; keywords, functions, containers and fields |
| Watching | yes; the change feed, polled every second |
| Explain, transactions, native export | no |

## Read-only

On a read-only connection only one `SELECT` runs, with no `;` and no
user-defined function. Stored procedures and triggers are never run. Edits
are refused. Use the account's read-only key, so the service refuses writes
too.

## Compatibility

Tested with the Azure Cosmos DB Linux emulator. Not yet tested against an Azure account.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/cosmos/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/cosmos"
```
