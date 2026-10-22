# tobira Chroma driver

The `chroma` driver lets [tobira](https://github.com/xavidop/tobira) connect to a
[Chroma](https://www.trychroma.com) server over the `chroma://` scheme. It is built into the `tobira` binary.

## Connect

```text
chroma://localhost:8000
chroma://localhost:8000?tenant=acme&database=prod
chroma://chroma.example.com:8000?tls=false&allow_plaintext=1
chroma://api.trychroma.com?tenant=…&database=…
```

Parameters: `tenant`, `database`, `tls`, `allow_plaintext`, `auth_header` and
`token` (prefer the connection's `password:` reference). The port defaults to
8000 over HTTP and 443 over HTTPS. TLS is on for any host that is not this
machine; plain HTTP to one needs `tls=false&allow_plaintext=1`, and Chroma
Cloud is always TLS.

## Quick example

A query is one JSON object, in the shape of Chroma's own get and query calls.

```sh
tobira query "chroma://localhost:8000" '{"op":"count","collection":"docs"}' --read-only
```

```json
{"collection":"docs","where":{"year":{"$gte":2020}},"whereDocument":{"$contains":"vector"},"limit":20}
{"collection":"docs","queryEmbeddings":[[0.1,0.2,0.3]],"nResults":5}
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, collections, describe | yes |
| Browse | yes; filters run on the server; sorting up to 10,000 records |
| Similarity search | yes; by vector, or by text with `ai.embedder` |
| Editing | yes; insert, update and delete of one record by id |
| Explain, transactions, completion, native export, watching | no |

## Read-only

Queries never write: only `get`, `query` and `count` run. On a read-only
connection every other operation (including Chroma's `search` API, which can
embed text on the server) is refused before anything is sent, and so are
edits. Query texts are embedded only by tobira's `ai.embedder`, never by
Chroma or an embedding function stored in the collection.

## Compatibility

Tested with Chroma 1.5, including token authentication. Not yet tested against Chroma Cloud.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/chroma/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/chroma"
```
