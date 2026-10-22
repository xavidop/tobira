---
layout: ../../../layouts/DocsLayout.astro
title: Qdrant
---

# Qdrant

The `qdrant` driver connects tobira to Qdrant, the vector database, over its gRPC API. It is
compiled into the `tobira` binary. You query with Qdrant's own requests in JSON, browse and edit
points, and run similarity search by vector or by text.

## Quick start

```sh
tobira query "qdrant://localhost" '{"op":"count","collection":"docs"}' --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: vectors
    driver: qdrant
    dsn: qdrant://xyz.cloud.qdrant.io
    password: env:QDRANT_API_KEY
    read_only: true
```

## Connecting

```text
qdrant://localhost
qdrant://qdrant.example.com:6334
qdrant://xyz.cloud.qdrant.io?tls=true
```

- The port is the gRPC port: 6334 by default, not the REST port 6333.
- **API key.** Put it in the connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`
  (see [Passwords and secrets](/docs/secrets/)), or in the DSN's password position
  (`qdrant://:KEY@host`; the user name is ignored). A key given as `?api_key=` is refused, so it
  can never be printed. Keys never appear in errors.
- A path is refused: each request names its collection.
- The only parameters, which the connection's `options:` may also set, are `tls=true|false` and
  `allow_plaintext=1`.

**TLS** is on by default for any host that is not this machine (`localhost` or a loopback
address), so nothing is sent in clear by accident. On this machine it is off unless you set
`tls=true`. To use plain text with another host anyway, set both `tls=false` and
`allow_plaintext=1`; with a key, tobira then writes a warning to the `--debug` log. Qdrant Cloud
(`*.cloud.qdrant.io`) refuses `tls=false` even with it. The driver speaks gRPC only, which has no
HTTP redirects.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, collections, describe | yes; one namespace, `default`; collections are Qdrant collections |
| Browse | yes; point-id order, or sorted by one payload field that has a range index |
| Similarity search | yes; by vector, or by text with an [embedder](/docs/ai/) |
| Editing | yes; insert, update and delete of single points by id |
| Explain, transactions, completion, native export, watching | no |

## Queries

A query is a JSON object with an `op`, a `collection` and the rest of that operation's gRPC
request, with the field names of Qdrant's `.proto` files:

| op | What it runs | Limits |
| --- | --- | --- |
| `scroll` | points in id order, with an optional filter | `limit` caps the total; without it every point is streamed |
| `search` | nearest neighbours of a vector | `limit` defaults to 10, at most 1000; `offset` at most 10,000 |
| `query` | Qdrant's universal query (nearest, recommend, order by, fusion …) | `limit` defaults to 10, at most 1000; `offset` at most 10,000; prefetch limits at most 1000 |
| `count` | the number of matching points | |
| `retrieve` | points by id | at most 1000 ids |

```json
{"op":"scroll","collection":"docs","filter":{"must":[{"field":{"key":"lang","match":{"keyword":"en"}}}]},"limit":20}
{"op":"search","collection":"docs","vector":[0.1,0.2,0.3],"limit":5}
{"op":"query","collection":"docs","query":{"nearest":{"dense":{"data":[0.1,0.2,0.3]}}},"using":"text","limit":5}
{"op":"retrieve","collection":"docs","ids":[{"num":"42"}],"with_vectors":{"enable":true}}
```

- `collection_name` works as well as `collection`.
- Payloads come back unless `with_payload` turns them off; vectors only when `with_vectors` asks.
- A limit or offset above its cap is cut to it, with a warning.
- Queries take no parameters.
- A query's columns come from its first page of points; payload keys first seen later are named
  in a warning.

## Points as rows

Each point is a row: `id`, one column per vector (`vector` for the default vector, otherwise the
vector's name), then one column per payload key. Search results add `_score`; similarity search
adds `_distance`.

- Payloads are JSON, so values show as numbers, text, booleans, lists and objects. Nested objects
  open as a JSON tree in the detail view.
- Dense vectors are vectors, sparse vectors are objects of `indices` and `values`, and
  multi-vectors are lists of vectors.
- A payload key named like one of tobira's columns (`id`, a vector, `_score`) or starting with
  `payload.` shows with a `payload.` prefix, so every key keeps its own column.

A payload field with a datetime index shows as a time. To show other fields with a type JSON
lacks, declare it in the collection's metadata:

```json
{"tobira": {"types": {"price": "decimal", "tags": "json", "seen": "time"}}}
```

The types are `string`, `int`, `float`, `bool`, `decimal`, `time` and `json`. A value that does not
fit its declared type shows as what it is, and the column then has a mixed type such as
`string|int`. When you edit, decimals are written as their exact digits (or stay numbers, when the
field held a number), times as RFC 3339 text in UTC and bytes as base64.

## Browsing

- There is one namespace, `default`. Collections are listed by name, at most 10,000.
- **Paging** follows point ids. A page is 100 points by default, at most 1000.
- **Sorting** takes one field: `id` ascending, or a payload field with a range index (integer,
  float or datetime). `id` descending is not supported. Points without a value for the sort field
  come last.
- **Filters**: equals and not equals, comparisons on numbers and times, `in` on text or integers,
  and is null or is not null (missing, null or `[]`). "Not equals" also matches points that do not
  have the field. Contains and starts with are not supported, because Qdrant matches whole words.
- A **raw filter** is a Qdrant `Filter` in JSON, combined with the other filters.

## Editing

Points are edited one at a time, by `id`. Columns named like a vector set that vector (a list of
numbers of the vector's size); every other column is a payload key. Writes wait for Qdrant to apply
them.

- **Insert** uses the `id`, or a random UUID when there is none. An id that is already taken is
  refused rather than overwritten, even when another client creates it at the same moment. To
  check this, tobira briefly writes a `_tobira_insert` payload key and removes it straight away; if
  the server becomes unreachable at that moment, the key can stay, and you can delete it by hand.
- **Update** sets payload keys (NULL stores null) and vectors together. A vector of the wrong size
  is refused before anything changes. The id cannot change.
- **Delete** removes the point.

Sparse and multi-vectors are shown but not edited.

## Similarity search

Press `S` on a row, use `:similar`, or ask the AI assistant.

- Search by a vector, or by text. A text search needs `ai.embedder` in the
  [AI settings](/docs/ai/): tobira turns the text into a vector, which must have the collection's
  size.
- When a collection has several vectors, tobira uses the one you name, or else the only dense
  vector, or else the only one whose distance matches the requested metric. Qdrant fixes a
  vector's distance when the collection is created, so another metric is refused.
- Results have a `_distance` column, nearest first: Euclid and Manhattan scores as they are,
  1 − score for cosine and −score for dot product.
- `k` defaults to 10, at most 1000.

## Read-only guard

Queries never write, on any connection: only the five operations above run, and the request is
parsed before tobira sends anything. On a read-only connection (`read_only: true`, or `--read-only`
on one command) these are refused:

- every other operation, including every write (upsert, delete, payload and vector changes,
  batches, field indexes, collections, aliases, snapshots, shard keys) and any other spelling
  (`Upsert`);
- a repeated key such as two `op`s, a missing `op`, trailing data, and any text that is not
  exactly one JSON object;
- a field the read request does not have, such as a write body (`points`, `operations`) on a
  `scroll`;
- a `document`, `image` or `object` vector input anywhere in the request (in `query`, `prefetch`,
  `recommend`, `discover` or `context`). On a Qdrant server set up for inference (such as Qdrant
  Cloud Inference) these ask the server to run an embedding model, which sends your text onward
  and can cost money. Search with a vector, or with text through tobira's own embedder
  ([`ai.embedder`](/docs/ai/configuration/)).

Editing is refused on read-only connections. A raw browse filter cannot write.

Qdrant also has read-only API keys (`QDRANT__SERVICE__READ_ONLY_API_KEY`). Use one for read-only
connections where you can: then the server refuses writes too.

## Limits

| Limit | Value |
| --- | --- |
| Collections listed | 10,000 |
| Browse page | 100 points by default, at most 1000 |
| `search` and `query` results | 1000 per call, `offset` at most 10,000 |
| `retrieve` | 1000 ids |
| Similarity search | 1000 neighbours |

## Compatibility

Tested with Qdrant 1.19.

## Troubleshooting

- **The TLS handshake fails, and the error says to add `tls=false&allow_plaintext=1`**: the server
  speaks plain gRPC. Add both on a trusted network, or turn on TLS in Qdrant.
- **Connection refused on port 6333**: that is the REST port. tobira uses gRPC on 6334.
- **"text similarity needs ai.embedder"**: set `ai.embedder` in the [AI settings](/docs/ai/), or
  search by vector.
- **Sorting by a field is not supported**: create a range payload index (integer, float or
  datetime) on it.
- **Writes fail with a permission error**: the connection uses a read-only API key.
