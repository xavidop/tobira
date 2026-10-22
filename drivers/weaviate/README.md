# tobira Weaviate driver

The Weaviate driver for [tobira](https://github.com/xavidop/tobira), built on
the official Go client (`github.com/weaviate/weaviate-go-client/v5`) over
REST and GraphQL. It registers the `weaviate` driver for the `weaviate://`
scheme and is compiled into the `tobira` binary.

## Connect

```text
weaviate://localhost:8080
weaviate://weaviate.example.com:8080?scheme=http&allow_plaintext=1
weaviate://xyz.weaviate.cloud
```

Parameters: `scheme` (`http` or `https`), `allow_plaintext` and `api_key`
(prefer the connection's `password:` reference). The scheme is `http` on this
machine and `https` elsewhere. `scheme=http` to another host also needs
`allow_plaintext=1`, and Weaviate Cloud is always HTTPS.

## Quick example

A query is a JSON request or one raw GraphQL read.

```sh
tobira query "weaviate://localhost:8080" '{"collection":"Article","limit":5}' --read-only
```

```json
{"collection":"Article","fields":["title","n"],"where":{"path":["n"],"operator":"GreaterThan","valueInt":2},"limit":10}
{"collection":"Article","nearVector":{"vector":[0.1,0.2,0.3]},"limit":5}
```

```graphql
{ Aggregate { Article { meta { count } } } }
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, collections, describe | yes; JSON requests or GraphQL `Get` and `Aggregate` |
| Browse | yes; id order, or filtered and sorted by properties |
| Similarity search | yes; by vector, or by text with `ai.embedder` |
| Editing | yes; insert, update and delete of one object by id |
| Explain, transactions, completion, native export, watching | no |

## Read-only

Queries never write. On a read-only connection tobira also refuses reads that
call a remote module, which would send your data out: `nearText` and `hybrid`
on a collection with a hosted vectorizer, `ask`, `generate`, `rerank` and
the like. Edits are refused too.

## Compatibility

Tested with Weaviate 1.40, with anonymous access and with API keys. Not yet tested: Weaviate
Cloud, multi-tenancy and OIDC sign-in.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/weaviate/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/weaviate"
```
