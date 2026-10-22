# tobira MongoDB driver

The MongoDB driver for [tobira](https://github.com/xavidop/tobira), built on
the official Go driver (`go.mongodb.org/mongo-driver/v2`). It registers the
`mongodb` driver for the `mongodb://` and `mongodb+srv://` schemes and is
compiled into the `tobira` binary.

## Connect

```text
mongodb://user@localhost:27017/shop
mongodb://user@db1,db2,db3/shop?replicaSet=rs0&authSource=admin
mongodb+srv://user@cluster0.example.net/shop
mongodb://user@db.example.com/shop?tls=true
```

Every URI option of the official driver works. The database in the path is
the default; without one it is `test`. A password from the connection's
`password:` reference replaces the one in the URI, and the URI must then name
the user. TLS is set with the URI's own options (`tls=true`, `tlsCAFile=…`).

## Quick example

A query is one MongoDB command document in JSON or extended JSON. The first
key names the command.

```sh
tobira query "mongodb://reader@localhost:27017/shop" \
  '{"find":"orders","filter":{"status":"paid"},"sort":{"_id":-1},"limit":10}' --read-only
```

```json
{"aggregate":"orders","pipeline":[{"$group":{"_id":"$status","n":{"$sum":1}}}]}
{"distinct":"orders","key":"status"}
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, collections, describe | yes; describe samples 100 documents |
| Browse | yes; paged by `_id`, or by skip when sorted |
| Editing | yes; insert, update and delete of one document, by `_id` |
| Explain | yes; the `explain` command, `executionStats` for analyze |
| Completion | yes; commands, operators, collections and field paths |
| Watching | yes; change streams, on a replica set or sharded cluster |
| Transactions, native export | no |

## Read-only

On a read-only connection only `find`, `aggregate`, `count`, `distinct`,
`listCollections`, `listIndexes`, `collStats`, `dbStats` and `explain` (of a
read) run. Write, admin and session commands, `$out` and `$merge` anywhere in
the document, and `linearizable` reads are refused. Edits are refused before
anything is sent.

## Compatibility

Tested with MongoDB 8, as a single-node replica set and as a standalone server.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/mongodb/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/mongodb"
```
