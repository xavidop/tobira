# tobira Pinecone driver

The Pinecone driver for [tobira](https://github.com/xavidop/tobira), built on
the official Go SDK (`github.com/pinecone-io/go-pinecone/v7`): the control
plane over REST, records over gRPC. It registers the `pinecone` driver for
the `pinecone://` scheme and is compiled into the `tobira` binary.

## Connect

```text
pinecone://                                                              # your project on api.pinecone.io
pinecone://?host=docs-abc123.svc.aped-4627-b74a.pinecone.io&index=docs   # one index
pinecone://localhost:5080?tls=false                                      # pinecone-local
pinecone://?host=localhost:5081&tls=false&index=docs&metric=cosine       # one pinecone-local index
```

Parameters: `host`, `index`, `metric`, `tls` and `allow_plaintext`. The API
key goes in the connection's `password:` reference. TLS is on unless
`tls=false`, which Pinecone's cloud (`*.pinecone.io`) refuses; plain HTTP to a
host that is not this machine also needs `allow_plaintext=1`.

## Quick example

A query is one JSON object: an `op` (`query`, `fetch`, `list` or `stats`) and
its members, in Pinecone's own spelling.

```sh
tobira query "pinecone://?host=docs-abc123.svc.aped-4627-b74a.pinecone.io&index=docs" \
  '{"op":"stats"}' --read-only
```

```json
{"namespace":"en","vector":[0.1,0.2,0.3,0.4],"topK":5,"filter":{"genre":{"$eq":"drama"}}}
{"op":"fetch","namespace":"en","ids":["doc-1","doc-2"]}
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, indexes, namespaces, describe | yes; indexes are namespaces, Pinecone namespaces are collections |
| Browse | yes; filters and sorting run in tobira |
| Similarity search | yes; by vector, or by text with `ai.embedder` |
| Editing | yes; insert, update and delete of one record |
| Explain, transactions, completion, native export, watching | no |

## Read-only

Queries never write: only the four read operations run. On a read-only
connection every other operation, a repeated `op` and a write member hidden in
a read are refused before anything is sent, and so are edits. tobira never
calls Pinecone's inference API. Use an API key whose role cannot write, too.

## Compatibility

Tested with Pinecone Local 0.7, an emulator. Not yet tested against the Pinecone service.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/pinecone/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/pinecone"
```
