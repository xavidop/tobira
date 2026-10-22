# tobira Elasticsearch and OpenSearch driver

The Elasticsearch and OpenSearch driver for
[tobira](https://github.com/xavidop/tobira), built on
`github.com/elastic/go-elasticsearch/v8`. It registers two drivers, both
compiled into the `tobira` binary:

- `elasticsearch`, for `elasticsearch://` DSNs;
- `opensearch`, for `opensearch://` DSNs.

## Connect

```text
elasticsearch://localhost
elasticsearch://elastic@es.example.com:9200?ca_file=/etc/es/http_ca.crt
elasticsearch://my-deployment.es.example.com:443
elasticsearch://localhost:9200?tls=false
opensearch://admin@search.example.com:9200
```

With a user, the password is used for basic auth. Without a user, the
password is an API key (encoded, or `id:key`). TLS is on by default; the port
defaults to 9200. Parameters: `tls`, `allow_plaintext`, `ca_file`,
`tls_skip_verify` and `allow_system_indices`. `tls=false` to another machine
also needs `allow_plaintext=1`, and Elastic Cloud and Amazon OpenSearch hosts
are always TLS.

## Quick example

A query is JSON: the search form names the index and gives a `_search` body;
the raw form is any REST call.

```sh
tobira query "elasticsearch://elastic@localhost:9200?ca_file=http_ca.crt" \
  '{"index":"articles","query":{"match":{"title":"database"}}}' --read-only
```

```json
{"index":"orders","size":0,"aggs":{"by_status":{"terms":{"field":"status"}}}}
{"method":"GET","path":"/_cat/indices?format=json"}
```

## Capabilities

| Capability | elasticsearch | opensearch |
| --- | --- | --- |
| Query, indexes, describe | yes | yes |
| Browse | yes; pages on a point in time | yes |
| Explain | yes; `_validate/query`; `ANALYZE` runs the search with profiling | yes |
| Editing | yes; one document at a time | yes |
| Similarity search | yes; kNN on `dense_vector` | no |
| Transactions, completion, native export, watching | no | no |

## Read-only

On a read-only connection tobira checks each request before it is sent. It
refuses scripts, inference (which would send your text to a model or an
inference endpoint), system and hidden indexes, and any raw call outside a
read allow-list (`_search`, `_count`, `_mget`, `_mapping`, `_cat` and a few
more). Edits are refused too.

## Compatibility

Tested with Elasticsearch 8.19 (TLS and a password, API keys, similarity search) and OpenSearch
2.19 (with the security plugin on and off).

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/elasticsearch/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/elasticsearch"
```
