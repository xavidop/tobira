# tobira Qdrant driver

The Qdrant driver for [tobira](https://github.com/xavidop/tobira), built on
`github.com/qdrant/go-client` over gRPC. It registers the `qdrant` driver for
the `qdrant://` scheme and is compiled into the `tobira` binary.

## Connect

```text
qdrant://localhost
qdrant://qdrant.example.com:6334
qdrant://xyz.cloud.qdrant.io?tls=true
```

The port is the gRPC port, 6334 by default. The API key goes in the
connection's `password:` reference (or the DSN's password position). The only
parameters are `tls` and `allow_plaintext`. TLS is on for any host that is not
this machine; plain text to one needs `tls=false&allow_plaintext=1`, and
Qdrant Cloud is always TLS.

## Quick example

A query is one JSON object: an `op` (`scroll`, `search`, `query`, `count` or
`retrieve`), a `collection` and the rest of that gRPC request.

```sh
tobira query "qdrant://localhost" '{"op":"count","collection":"docs"}' --read-only
```

```json
{"op":"scroll","collection":"docs","filter":{"must":[{"field":{"key":"lang","match":{"keyword":"en"}}}]},"limit":20}
{"op":"query","collection":"docs","query":{"nearest":{"dense":{"data":[0.1,0.2,0.3]}}},"using":"text","limit":5}
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, collections, describe | yes |
| Browse | yes; point-id order, or one payload field with a range index |
| Similarity search | yes; by vector, or by text with `ai.embedder` |
| Editing | yes; insert, update and delete of one point by id |
| Explain, transactions, completion, native export, watching | no |

## Read-only

Queries never write: only the five read operations run. On a read-only
connection every other operation, a repeated `op` and a write body hidden in
a read request are refused before anything is sent, and so are edits and any
`document`, `image` or `object` vector input (server-side inference). Use a
Qdrant read-only API key too.

## Compatibility

Tested with Qdrant 1.19.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/qdrant/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/qdrant"
```
