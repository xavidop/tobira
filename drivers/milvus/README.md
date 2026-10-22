# tobira Milvus driver

The `milvus` driver lets [tobira](https://github.com/xavidop/tobira) connect to a
[Milvus](https://milvus.io) server over the `milvus://` scheme. It is built into the `tobira`
binary.

## Connect

```text
milvus://localhost
milvus://localhost:19530/analytics
milvus://root@milvus.example.com:19530/default
milvus://in03-xxxx.api.gcp-us-west1.zillizcloud.com:443
```

The port defaults to 19530 and the path is the database (`default` when left
out). The password (or a Zilliz Cloud API key) goes in the connection's
`password:` reference. The only parameters are `tls` and `allow_plaintext`.
TLS is on for any host that is not this machine; plain text to one needs
`tls=false&allow_plaintext=1`, and Zilliz Cloud is always TLS.

## Quick example

A query is one JSON object; its `op` is `query`, `get`, `count` or `search`.

```sh
tobira query "milvus://localhost:19530" '{"op":"count","collection":"docs"}' --read-only
```

```json
{"collection":"docs","filter":"lang == {lang} and year >= 2020","params":{"lang":"en"},"outputFields":["title"],"limit":20}
{"collection":"docs","annsField":"embedding","vector":[0.1,0.2,0.3],"limit":5}
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, collections, describe | yes |
| Browse | yes; primary-key order, or sorted by any field up to 10,000 entities |
| Similarity search | yes; by vector, by text through a BM25 or TEXTEMBEDDING function, or with `ai.embedder` |
| Editing | yes; insert, update and delete of one entity by primary key |
| Explain, transactions, completion, native export, watching | no |

## Read-only

Queries never write: only the four read operations run. On a read-only
connection tobira also never loads a collection, and refuses what would send
data to an outside provider: a text search through a TEXTEMBEDDING function
and a `model` reranker. Edits are refused too.

## Compatibility

Tested with Milvus 2.6 standalone, with authorization on. Not yet tested: Zilliz Cloud, Milvus
clusters and servers before 2.6.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/milvus/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/milvus"
```
