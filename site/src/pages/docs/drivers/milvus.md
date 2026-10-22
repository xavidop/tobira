---
layout: ../../../layouts/DocsLayout.astro
title: Milvus
---

# Milvus

The `milvus` driver connects tobira to Milvus, the vector database, over its gRPC API. It is
compiled into the `tobira` binary. You query with JSON requests and Milvus filter expressions,
browse and edit entities, and run similarity search by vector or by text.

## Quick start

```sh
tobira query "milvus://localhost:19530" '{"op":"count","collection":"docs"}' --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: vectors
    driver: milvus
    dsn: milvus://root@milvus.example.com:19530/default
    password: env:MILVUS_PASSWORD
    read_only: true
```

## Connecting

```text
milvus://localhost
milvus://localhost:19530/analytics
milvus://root@milvus.example.com:19530/default
milvus://in03-xxxx.api.gcp-us-west1.zillizcloud.com:443
```

- The port is the gRPC port, 19530 by default. Zilliz Cloud endpoints name theirs (often 443).
- The path is the database, `default` when left out. Every database you may list is a namespace.
- **Token.** For a Milvus user, put the user in the DSN and the password in the connection's
  `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD` (see
  [Passwords and secrets](/docs/secrets/)). For Zilliz Cloud, the API key alone is the password.
  A token, key or password given as a parameter (`?token=`) is refused, so it can never be
  printed. Tokens never appear in errors.
- The only parameters, which the connection's `options:` may also set, are `tls=true|false` and
  `allow_plaintext=1`.
- Open lists the database's collections once, so a bad address, token or database fails there.

**TLS** is on by default for any host that is not this machine (`localhost` or a loopback
address), and off on this machine unless `tls=true`. It verifies the server's certificate against
the system roots. To use plain text with another host anyway, set both `tls=false` and
`allow_plaintext=1`; with a token, tobira then writes a warning to the `--debug` log. Zilliz Cloud
(`*.zillizcloud.com`, `*.zilliz.com`, `*.zillizcloud.com.cn`) refuses `tls=false` even with it.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, collections, describe | yes; namespaces are databases; collections are Milvus collections |
| Browse | yes; primary-key order, or sorted by any field (up to 10,000 entities) |
| Similarity search | yes; by vector, by text through a BM25 or TEXTEMBEDDING function, or by text with an [embedder](/docs/ai/) |
| Editing | yes; insert, update and delete of single entities by primary key |
| Explain, transactions, completion, native export, watching | no |

## Queries

A query is one JSON object. The `op` is `query`, `get`, `count` or `search`. Without one, a request
with a `vector` or a `text` is a search, one with `ids` a get, and anything else a query.

| op | What it runs | Limits |
| --- | --- | --- |
| `query` | entities matching a boolean `filter`, in primary-key order | `limit` caps the total; without it every match is streamed |
| `get` | entities by primary key | at most 1000 ids, all integers or all strings |
| `count` | one row, `count` | |
| `search` | nearest neighbours of a `vector`, or a `text` for a BM25 or TEXTEMBEDDING field | `limit` defaults to 10, at most 1000 |

```json
{"collection":"docs","filter":"lang == {lang} and year >= 2020","params":{"lang":"en"},"outputFields":["title"],"limit":20}
{"op":"get","collection":"docs","ids":[42,43]}
{"op":"count","collection":"docs","filter":"json_contains(tags, \"beta\")"}
{"collection":"docs","annsField":"embedding","vector":[0.1,0.2,0.3],"limit":5,"searchParams":{"ef":64}}
{"collection":"docs","text":"vector database","annsField":"sparse","limit":5}
{"collection":"docs","vector":[0.1,0.2],"rerank":{"inputFields":["published"],"params":{"reranker":"decay","function":"gauss","origin":0,"scale":86400}}}
```

| Member | Meaning |
| --- | --- |
| `collection` | required |
| `database` | another database than the connection's |
| `filter` | a Milvus boolean expression |
| `params` | values for `{name}` placeholders in the filter: strings, numbers, booleans or lists of one of those |
| `outputFields`, `partitions`, `offset`, `limit` | as in Milvus |
| `consistency` | `strong` (the default, so you see your own edits), `bounded`, `session` or `eventually` |
| `annsField`, `vector`, `text`, `searchParams` | for a search; `searchParams` holds `ef`, `nprobe`, `radius` and the like |
| `rerank` | one function reranker: `inputFields` and `params`, with its `reranker` |

- `params` keep values out of the expression text, so they never need quoting by hand.
  Positional query parameters bind `{p1}`, `{p2}` ….
- Searches add a `_score` column with Milvus's own score.
- Unknown members, a duplicate key, trailing data and wrong types are refused.

## Entities as rows

Each entity is a row: the primary key, the other fields in schema order, then the keys of the
dynamic field.

| Milvus | tobira |
| --- | --- |
| Bool | bool |
| Int8, Int16, Int32, Int64 | int |
| Float, Double | float |
| VarChar | string |
| JSON | JSON |
| Timestamptz | time (UTC) |
| Geometry | string (WKT) |
| Array | list |
| FloatVector, Float16Vector, BFloat16Vector | vector |
| BinaryVector | bytes |
| Int8Vector | list of int |
| SparseFloatVector | object of `indices` and `values` |
| struct and vector arrays | string (their JSON) |

- Milvus has no decimal type: a decimal is stored as a Double.
- Dynamic keys come from the `$meta` field. A key named like a field or like one of tobira's
  columns (`_score`, `_distance`), or starting with `$meta.`, shows with a `$meta.` prefix, so
  every key keeps its own column and edits reach it.
- **Describe** lists the fields (function outputs as generated), the indexes, the row count and
  the schema JSON. Function parameters that look like credentials (`api_key`, `token` …) are
  masked.

## Browsing

- **Paging** follows the primary key. A page is 100 entities by default, at most 1000.
- **Sorting** takes one field, which tobira applies itself: it reads every matching entity (at
  most 10,000; add a filter for more), sorts them with NULLs last and pages through them. Vectors
  cannot be sorted by.
- **Filters**: equality, comparisons, `in`, and is null or is not null, on fields and dynamic keys
  (for a dynamic key, is null means the key is absent). Values are sent as data, never spliced
  into the expression, so a value holding quotes or operators matches literally.
- Contains and starts with on text are `LIKE` patterns. A value holding `%` or a backslash is not
  supported, because Milvus's `LIKE` cannot match those exactly.
- A **raw filter** is a Milvus expression, combined with the other filters.

## Editing

Entities are edited one at a time, by primary key. Columns named like a field set that field; any
other column is a dynamic key.

- **Insert** needs the primary key, unless the collection uses auto ids, which Milvus assigns. A
  key that is already taken is refused (Milvus itself would overwrite it). Fields left out are
  NULL or their default.
- **Update** sends only the changed fields. NULL sets a nullable field to null. Entities of an
  auto-id collection cannot be updated: Milvus would give them a new key, so delete and insert
  instead.
- **Delete** removes the entity.
- Milvus has no conditional writes: tobira checks, then writes, so a concurrent change by another
  client in between is not detected.

Fields computed by a function, and sparse, binary, half-precision and int8 vectors, are shown but
not edited. Inserting into a collection with a TEXTEMBEDDING function makes Milvus call its
embedding provider.

## Similarity search

Press `S` on a row, use `:similar`, or ask the AI assistant.

- A vector search uses a dense float vector field. A text search uses the collection's BM25 or
  TEXTEMBEDDING output field when it has one (Milvus embeds the text), else `ai.embedder` from the
  [AI settings](/docs/ai/).
- With several candidate fields, tobira uses the one you name, or the only one whose index metric
  matches the requested metric. Milvus fixes the metric when the index is built, so another
  metric is refused.
- Results have a `_distance` column, nearest first: L2 as it is, 1 − score for COSINE, −score for
  IP and BM25. `k` defaults to 10, at most 1000.

## Read-only guard

Queries never write, on any connection: only the four operations above run, and the request is
parsed before tobira sends anything. On a read-only connection (`read_only: true`, or `--read-only`
on one command) these are refused:

- every other operation: `insert`, `upsert`, `delete`, `load`, `release`, `flush`, `compact`,
  index, collection, database and user operations, `hybrid_search`, `run_analyzer`, and any other
  spelling;
- a repeated key such as two `op`s, a missing collection, an unknown member, trailing data, and any
  text that is not exactly one JSON object;
- a filter tobira cannot read: an unknown character (`;`, a backtick, `#`, `@`), an unterminated
  string, unbalanced brackets, more than 64 KiB, or a call of a function outside the read-only
  list (`json_contains*`, `array_contains*`, `array_length`, `text_match`, `phrase_match`,
  `random_sample` and the `st_*` geometry functions).

**Collections are never loaded.** Milvus answers queries only on loaded collections, and loading
changes the server's state. A read-only connection tells you the collection is not loaded, and the
collection list marks it `not loaded`. Load it from a writable connection (the first read there
loads it) or another client.

**Nothing sent to outside services.** A read-only connection refuses what would send your data to an outside provider:

- a text search of a field produced by a TEXTEMBEDDING function, which sends the text to OpenAI,
  Cohere, TEI and the like. A vector search of the same field, and a BM25 text search, which runs
  inside Milvus, are allowed;
- any reranker but `decay` and `boost`, which run inside Milvus. A `model` reranker sends the
  query and documents out. A read-only `decay` or `boost` takes only its own parameters, so a
  `provider` or an `endpoint` is refused.

Editing is refused on read-only connections. Where you can, also connect read-only connections as
a Milvus user with read privileges only.

## Limits

| Limit | Value |
| --- | --- |
| `get` | 1000 ids |
| `search` and similarity search | 1000 results |
| Browse page | 100 entities by default, at most 1000 |
| Sorted browsing | 10,000 entities |
| Filter expression | 64 KiB |
| Reply size | 64 MiB |

## Compatibility

Tested with Milvus 2.6, run standalone with authorization on, as the root user. Not yet tested:
Zilliz Cloud, Milvus clusters, users with restricted privileges, and servers before 2.6 (partial
updates, `Timestamptz` and nullable vectors need 2.6).

## Troubleshooting

- **The collection is not loaded**: load it from a writable connection or another client, then
  query it read-only.
- **A collection cannot be loaded**: a vector field has no index. Build one first.
- **"read-only: field … is produced by the … function …, which sends the query text to an outside
  embedding provider"**: search with a vector, or from a writable connection.
- **"read-only: the … reranker calls an outside model provider"**: use `decay` or `boost`, or a
  writable connection.
- **A reply is too large**: narrow `outputFields` (vectors are large) or lower the `limit`.
- **Updating fails on an auto-id collection**: delete the entity and insert it again.
