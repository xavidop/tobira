---
layout: ../../../layouts/DocsLayout.astro
title: Elasticsearch and OpenSearch
---

# Elasticsearch and OpenSearch

tobira talks to Elasticsearch and OpenSearch over their REST API. Two drivers are compiled into
the `tobira` binary: `elasticsearch`, for `elasticsearch://` DSNs, and `opensearch`, for
`opensearch://` DSNs. Collections are indexes, query results carry a `_score` column, and on
Elasticsearch you can run similarity search on `dense_vector` fields.

## Quick start

```sh
tobira query "elasticsearch://elastic@localhost:9200?ca_file=http_ca.crt" \
  '{"index":"articles","query":{"match":{"title":"database"}}}' --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: search
    driver: elasticsearch
    dsn: elasticsearch://elastic@es.example.com:9200?ca_file=/etc/es/http_ca.crt
    password: env:SEARCH_ES_PASSWORD
    read_only: true
```

## Connecting

```text
elasticsearch://localhost
elasticsearch://elastic@es.example.com:9200?ca_file=/etc/es/http_ca.crt
elasticsearch://my-deployment.es.example.com:443
elasticsearch://localhost:9200?tls=false
opensearch://admin@search.example.com:9200
```

- The port defaults to 9200. A DSN has no path.
- **Authentication.** With a user, the password is used for basic auth. Without a user, the
  password is an API key, in its encoded (base64) form or as `id:key`. Put it in the connection's
  `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD` (see
  [Passwords and secrets](/docs/secrets/)). A secret given as `?api_key=`, `?password=` or
  `?token=` is refused, so it can never be printed. Secrets never appear in errors.
- **TLS** is on by default. `ca_file=` names the CA of a cluster with its own, such as
  Elasticsearch 8's generated `http_ca.crt`.
- **Plain HTTP.** `tls=false` works on this machine (`localhost` or a loopback address). To
  another host it also needs `allow_plaintext=1`. These managed hosts are always TLS, even with
  `allow_plaintext=1`: `*.elastic-cloud.com`, `*.cloud.es.io`, `*.elastic.cloud`,
  `*.es.amazonaws.com`, `*.es.amazonaws.com.cn` and `*.aoss.amazonaws.com`.
- tobira never follows an HTTP redirect, so credentials cannot be forwarded to another host.
  The parameters below can also be set in the connection's `options:`.

| Parameter | Meaning |
| --- | --- |
| `tls=true\|false` | TLS, on by default |
| `allow_plaintext=1` | allow `tls=false` to a host that is not this machine |
| `ca_file=/path/ca.pem` | trust this CA |
| `tls_skip_verify=true` | skip certificate verification (testing only); writes a warning to the `--debug` log |
| `allow_system_indices=1` | let read-only queries reach system, hidden and dot-prefixed indexes |

## Capabilities

| Capability | elasticsearch | opensearch |
| --- | --- | --- |
| Query, indexes, describe | yes; one namespace, `default`; collections are indexes, data streams and aliases | yes |
| Browse | yes; consistent pages on a point in time | yes |
| Explain | yes; `_validate/query`, which never runs the query; `ANALYZE` runs the search with profiling | yes |
| Editing | yes; insert, update and delete of single documents | yes |
| Similarity search | yes; kNN on `dense_vector`, by vector or by text with an [embedder](/docs/ai/) | no |
| Transactions, completion, native export, watching | no | no |

Use `opensearch://` for OpenSearch. Its API differs from Elasticsearch's: it sends no Elastic
product header, its point-in-time API differs, and its k-NN plugin has another shape, so tobira
has no similarity search for it. Pointed at OpenSearch, the `elasticsearch` driver fails with a
hint to use `opensearch://`.

## Queries

A query is one JSON object in one of two forms.

**The search form** names the index (a name, pattern or comma list) and gives the rest of a
`_search` body:

```json
{"index":"articles","query":{"multi_match":{"query":"database","fields":["title","body"]}}}
{"index":"logs-*","query":{"range":{"@timestamp":{"gte":"now-1h"}}},"sort":[{"@timestamp":"desc"}],"size":50}
{"index":"orders","size":0,"aggs":{"by_status":{"terms":{"field":"status"}}}}
```

- Without `size`, every hit streams, 1000 per page, through a point in time, up to the editor's
  row limit. The result is marked truncated when more hits existed.
- With `size`, at most that many hits come back.
- A request with `from`, `search_after`, `pit`, `collapse`, `rescore`, `knn` or `suggest` runs as
  one page (10 hits without `size`, as on the server).
- With `"size": 0`, the aggregations come back as one row, one column per aggregation.

**The raw form** is any REST call:

```json
{"method":"GET","path":"/_cat/indices?format=json&h=index,docs.count"}
{"method":"POST","path":"/orders/_count","body":{"query":{"term":{"status":"paid"}}}}
{"method":"POST","path":"/_msearch","body":[{"index":"a"},{"query":{"match_all":{}}}]}
```

A body is an object; an array of objects is sent as NDJSON (for `_msearch` and `_bulk`). Hits
become rows, a JSON array becomes rows of its objects, any other object becomes one row, and plain
text (a `_cat` call without `format=json`) becomes one row per line. A by-query write reports its
count as rows affected.

A path holds letters, digits and `- _ . + * , :` only. Empty, `.` and `..` segments and
percent-encoding are refused. Queries take no positional parameters.

### Documents as rows

Each hit is a row: `_id`, then `_index` when the search spans several indexes, then `_score`, then
one column per source field, in the order the first hits hold them. A field first seen on a later
page is not shown.

| Mapping type | tobira |
| --- | --- |
| `long`, `integer`, `short`, `byte`, `unsigned_long` | int; an `unsigned_long` beyond 64 bits is a decimal |
| `double`, `float`, `half_float` | float |
| `scaled_float` | decimal, with the source's exact digits |
| `boolean` | bool |
| `date`, `date_nanos` | time (UTC); other date formats show as text |
| `text`, `keyword` and other string types | string |
| `object` | object |
| `object` with `"enabled": false`, `flattened`, geo and range types | JSON |
| `nested` | list of objects |
| `dense_vector` | vector |
| `binary` | bytes |

Every field can also hold a list of its type. When a pattern or alias spans indexes that map a
field differently, the column takes the first index's type, so it can hold mixed values.

## Browsing

- **Collections** are indexes (open and closed, with document counts), data streams and aliases
  (shown as views). Hidden indexes and names starting with `.` are left out.
- **Describe** reads the mapping: `_id` first, as the key, then every mapped field with its nested
  properties. Multi-fields are listed in the field's comment.
- **Paging** uses `search_after` on a point in time, so pages stay consistent while documents
  change. The point in time closes on the last page or when the connection closes; otherwise it
  expires five minutes after the last page read. Reload to browse again after that.
- **Sorting** a `text` field uses its keyword multi-field. Text without one, disabled objects,
  vectors and binary fields cannot be sorted. Missing values sort last.

Filters on a `text` field go through its keyword multi-field when it has one; otherwise they are
phrase matches.

| Filter | Query |
| --- | --- |
| equals | `term` |
| not equals | `must_not` `term` (also matches documents without the field) |
| comparisons | `range` |
| contains | a case-sensitive `wildcard` on keyword fields; a phrase match on text |
| starts with | `prefix` on keyword fields; `match_phrase_prefix` on text |
| in | `terms` |
| is null, is not null | `exists` |

Contains and starts with on numbers and dates are not supported. A raw filter is one query clause,
such as `{"match":{"title":"tui"}}`, combined with the rest.

## Editing

- **Insert** with an `_id` creates the document with that id and never overwrites an existing one.
  Without an `_id`, the server picks one.
- **Update** is partial: the other fields stay. `_id` cannot change.
- A key other than `_id` must match exactly one document, and the write fails with a conflict if
  that document changed meanwhile. Every write waits for a refresh, so the next search sees it.

## Explain

`tobira query --explain` validates the search with `_validate/query` and shows how the server
rewrites it, without running it. With analyze, it runs the search with the profile API and shows
the profile. The search form and raw `_search` and `_count` calls can be explained.

## Similarity search (Elasticsearch)

Press `S` on a row, use `:similar`, or ask the AI assistant. The search runs on the index's
`dense_vector` field, or the one you name.

- With the field's own similarity (or the default) on an indexed field, tobira runs an approximate
  kNN search, with `num_candidates` of max(100, 10 × k).
- Another metric (L2, cosine or inner product) runs an exact `script_score` over the matching
  documents.
- Results have a `_distance` column, nearest first: the Euclidean distance, 1 − cosine similarity,
  or the negated inner product.
- At most 1000 neighbours are returned. Filters work as in browsing.
- A text search needs `ai.embedder`, which turns your text into a vector before the search.

## Read-only guard

Elasticsearch has no read-only session, so tobira checks the request before sending it. On a
read-only connection, and for the AI assistant, it refuses:

- **Scripts**: any key containing `script`, anywhere in the request (`script_fields`,
  `script_score`, runtime-mapping scripts, `scripted_metric` and so on).
- **Inference**: `query_vector_builder`, `inference_id`, `model_id`, `semantic`, `text_expansion`
  and the `text_similarity_reranker` retriever, at any depth. They run a model or an inference
  endpoint, possibly a paid third party that receives your text, so they send your data out and may cost money.
  `sparse_vector` with an explicit `query_vector` runs no model and passes. A field that happens to
  have one of these names is refused too.
- **System, hidden and dot-prefixed indexes** (`.security*`), in the target, the path or an
  `index` value in the body, and `expand_wildcards=all|hidden`. An exclusion such as `-.old`
  passes. `allow_system_indices=1` lifts this rule; the server's own privileges still apply.
- Duplicate keys, and text that is not exactly one JSON object.
- Any raw call outside this allow-list:

| Endpoint | Methods |
| --- | --- |
| `/` | GET |
| `[/<target>]/_search`, `_msearch`, `_count`, `_field_caps`, `_mget`, `_validate/query` | GET, POST |
| `[/<target>]/_mapping`, `[/<target>]/_mapping/field/<f>` | GET |
| `/<index>/_doc/<id>`, `/<index>/_source/<id>` | GET |
| `/_cat/...` | GET |
| `/_cluster/health[/<target>]` | GET |

So `_update_by_query`, `_delete_by_query`, `_reindex`, `_bulk`, document writes, mapping changes,
search templates, scrolls and administration are all refused. A read may carry only known read
parameters (`size`, `q`, `sort`, `filter_path`, `format`, `h` …), so `source=` cannot smuggle a
body past the checks. Editing is refused on a read-only connection.

For defence in depth, connect read-only with an API key or role limited to `read` and
`view_index_metadata`, so the server refuses writes too.

## Limits

A streamed search reads 1000 hits per page. Similarity search returns at most 1000 neighbours. A
response larger than 64 MiB is refused. A point in time expires 5 minutes after the last page read.

## Compatibility

- `elasticsearch`: tested with Elasticsearch 8.19.23, with security on (TLS and a password), API
  keys and similarity search.
- `opensearch`: tested with OpenSearch 2.19.7, with the security plugin on (TLS verified against
  its demo CA, basic auth) and off (plain HTTP).

## Troubleshooting

- **Certificate errors against Elasticsearch 8**: it signs its HTTPS certificate with its own CA.
  Copy `config/certs/http_ca.crt` from the server and add `ca_file=` with its path.
- **"the server is not Elasticsearch …; for OpenSearch use an opensearch:// DSN"**: change the scheme.
- **"the point in time expired"**: reload the view to browse again.
- **A read-only query is refused for a script or inference key**: run it on a writable connection,
  or rewrite it without the script.
- **A dot-prefixed index is refused on a read-only connection**: add `allow_system_indices=1`.
