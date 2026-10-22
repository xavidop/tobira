# tobira Cloud Firestore driver

The Cloud Firestore (Firebase, native mode) driver for
[tobira](https://github.com/xavidop/tobira), built on
`cloud.google.com/go/firestore`. It registers the `firestore` driver for the
`firestore://` scheme and is compiled into the `tobira` binary.

## Connect

```text
firestore://my-project
firestore://my-project?database=orders
firestore://demo-project?emulator=localhost:8080
```

The host is the project id. `database` picks a database (`(default)` when
left out); `emulator=host:port` talks plain gRPC to the Firestore emulator.
The service-account key JSON goes in the connection's `password:` reference,
never in the DSN; without one, Application Default Credentials are used.
Connections to Google always use TLS.

## Quick example

A query is one JSON request.

```sh
tobira query "firestore://my-project" \
  '{"collection":"orders","where":[["status","==","paid"]],"limit":10}' --read-only
```

```json
{"collectionGroup":"orders","where":[["user","==","ada"]],"orderBy":[["total","desc"]]}
{"collection":"orders","where":[["status","==","paid"]],"count":true}
{"document":"users/ada"}
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, database, collections, describe | yes; describe samples 100 documents |
| Browse | yes; `where` filters and `orderBy` |
| Editing | yes; create, update and delete of one document |
| Completion | yes; request keys, operators and root collections |
| Watching | yes; snapshot listeners on a collection or a document |
| Explain, transactions, native export | no |

## Read-only

A request can only read: its keys are an allow-list, and the whole text is
parsed before anything is sent. On a read-only connection any other key (a
write verb such as `set` or `delete`), a repeated key or trailing text is
refused, and so are edits.

## Compatibility

Tested against the Firestore emulator; not yet tested against Google Cloud.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/firestore/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/firestore"
```
