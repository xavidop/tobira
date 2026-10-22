---
layout: ../../../layouts/DocsLayout.astro
title: Pinecone
---

# Pinecone

The `pinecone` driver connects tobira to Pinecone, the managed vector database, through its
official Go SDK. It is compiled into the `tobira` binary. You query with Pinecone's own requests in
JSON, browse and edit records, and run similarity search by vector or by text.

## Quick start

```sh
tobira query "pinecone://?host=docs-abc123.svc.aped-4627-b74a.pinecone.io&index=docs" \
  '{"op":"stats"}' --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: pinecone
    driver: pinecone
    dsn: pinecone://
    password: env:PINECONE_KEY
    read_only: true
```

## Connecting

```text
pinecone://                                                              # your project on api.pinecone.io
pinecone://?host=docs-abc123.svc.aped-4627-b74a.pinecone.io&index=docs   # one index
pinecone://localhost:5080?tls=false                                      # pinecone-local
pinecone://?host=localhost:5081&tls=false&index=docs&metric=cosine       # one pinecone-local index
```

- **A project** (no `host=`): tobira lists every index of the project and finds each index's host
  through the control plane, `api.pinecone.io` unless the DSN names another host.
- **One index** (`host=`, the index host from the Pinecone console without `https://`): tobira
  talks to that index only, so an API key scoped to one index is enough.
- Unknown parameters, a path, or both a DSN host and `host=` are refused. The connection's
  `options:` may set `tls`, `allow_plaintext`, `host`, `index` and `metric`.

| Parameter | Meaning |
| --- | --- |
| `host` | one index host, `host[:port]` |
| `index` | the index's name in tobira, with `host=` (`default` when left out) |
| `metric` | `cosine`, `euclidean` or `dotproduct`, when the server does not report it (pinecone-local never does); similarity search needs it |
| `tls=false` | plain HTTP and gRPC |
| `allow_plaintext=1` | allow `tls=false` to a host that is not this machine |

- **API key.** Put it in the connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`
  (see [Passwords and secrets](/docs/secrets/)). It may also sit in the DSN as `pinecone://:KEY@…`
  or `?api_key=KEY`; tobira masks all of these wherever it prints a DSN, and the key never appears
  in errors. The `PINECONE_API_KEY` environment variable is not read.
- **TLS** is on unless `tls=false`. `tls=false` is refused for Pinecone's cloud (`pinecone.io` and
  `*.pinecone.io`), even with `allow_plaintext=1`. Plain HTTP to a host that is not this machine
  needs `allow_plaintext=1` too, and sending a key in clear there writes a warning to the
  `--debug` log.
- Index hosts found through the control plane always use TLS, except on this machine behind a
  plain-text control plane (pinecone-local).
- tobira never follows an HTTP redirect, so the key cannot be forwarded to another host.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, indexes, namespaces, describe | yes; indexes are namespaces, Pinecone namespaces are collections |
| Browse | yes; records in Pinecone's list order; filters and sorting run in tobira |
| Similarity search | yes; by vector, or by text with an [embedder](/docs/ai/) |
| Editing | yes; insert, update and delete of single records |
| Explain, transactions, completion, native export, watching | no |

## Queries

A query is a JSON object in Pinecone's own spelling. `op` is `query` (the default), `fetch`,
`list` or `stats`. `index` defaults to the current index and `namespace` to the default namespace.

| op | What it runs | Limits |
| --- | --- | --- |
| `query` | nearest records to exactly one of a `vector`, a record `id` or a `sparseVector`, with an optional `filter` | `topK` defaults to 10, at most 1000 |
| `fetch` | records by `ids`, in the order given; missing ids are skipped | 1 to 1000 ids |
| `list` | every record of a namespace, optionally by id `prefix` | `limit` caps the total |
| `stats` | one row per namespace: `namespace`, `vector_count` | |

```json
{"index":"docs","namespace":"en","vector":[0.1,0.2,0.3,0.4],"topK":5,"filter":{"genre":{"$eq":"drama"}}}
{"op":"query","namespace":"en","id":"doc-42","topK":3,"includeValues":true}
{"op":"fetch","namespace":"en","ids":["doc-1","doc-2"]}
{"op":"list","namespace":"en","prefix":"doc-","limit":50}
{"op":"stats","index":"docs"}
```

- `filter` is a Pinecone metadata filter, sent as is.
- Metadata comes back unless `includeMetadata` is false; vectors only with `includeValues`.
- A `topK` above 1000 is cut to it, with a warning.
- A query on a namespace that does not exist returns nothing, as in Pinecone.
- Queries take no parameters.

## Records as rows

Each record is a row: `_id`, `values` (the dense vector), `sparse_values` when the index has them,
then one column per metadata key. Query results add `_score`; similarity searches add `_distance`.

- A metadata key named like one of those columns, or starting with `meta_`, shows with a `meta_`
  prefix (key `values` is column `meta_values`), with a warning. Editing that column changes the
  metadata key, never the vector.
- Pinecone metadata holds text, numbers, booleans and lists of text. Numbers are stored as 64-bit
  floats: whole numbers show as integers (so `2.0` shows as `2`), others as floats.
- When you edit, times are written as RFC 3339 text in UTC, decimals as numbers when a float holds
  them exactly, and JSON as the value it holds. Objects, bytes, lists of anything but text, NaN and
  integers beyond 2^53 are refused, because Pinecone cannot store them. Pinecone has no null.

## Browsing

- **Indexes** are the namespaces in tobira, listed by name. **Pinecone namespaces** are the
  collections, with their record counts; the default namespace shows as `__default__`.
- **Paging.** Pinecone lists record ids but cannot filter or sort a listing, so tobira does it as
  it reads. Unsorted pages follow Pinecone's list order (100 records by default, at most 1000). A
  filtered page looks at up to 10,000 records, then ends with a warning and continues on the next
  page.
- **Sorting** (by `_id` or one metadata key) reads and sorts the whole namespace in tobira, up to
  10,000 records. Records without the field come last. Larger namespaces can be browsed unsorted
  or queried with a filter.
- **Filters**: equals, not equals, `in`, comparisons on numbers and text, contains (a substring of
  text, or a member of a list), starts with on text, and is null or is not null (the key is
  missing). Any of them may use `_id`. A raw filter is not supported: use a `query` with a Pinecone
  filter.
- Listing works on serverless indexes only.

## Editing

Records are edited one at a time, by `_id`, or by a metadata value that only one record has (such
as `sku`; this lookup needs a dense index). The `values` column sets the vector; every other column
is a metadata key.

- **Insert** needs the vector in `values`. Without an `_id`, the record gets a random UUID. An id
  that is already taken is refused, but Pinecone cannot refuse it itself: a record another client
  creates with the same id at the same moment is overwritten.
- **Update** changes the keys you edit and keeps the others. Pinecone cannot remove a metadata key
  in an update, so setting one to NULL is refused. The id cannot change.
- **Delete** removes the record.
- Pinecone has no conditional update or delete: if another client deletes a record while you edit
  or delete it, tobira reports success and nothing changes.

Sparse values are shown but not edited.

## Similarity search

Press `S` on a row, use `:similar`, or ask the AI assistant.

- The search runs on the namespace's dense vector. A sparse index is searched with a
  `sparseVector` query instead.
- A text search needs `ai.embedder` in the [AI settings](/docs/ai/). tobira turns the text into a
  vector, which must have the index's dimension.
- Pinecone fixes the metric when an index is created, so another metric is refused, and so is a
  search on an index whose metric is unknown (set `metric=`).
- Results have a `_distance` column, nearest first: the Euclidean score as Pinecone returns it,
  1 − score for cosine, −score for dot product. `k` defaults to 10, at most 1000.
- The filter runs in Pinecone. Contains, starts with and conditions on `_id` are not supported
  here. A raw filter is a Pinecone filter object, sent as is.

## Read-only guard

Queries never write, on any connection: only the four operations above run, and the request is
parsed before tobira sends anything. On a read-only connection (`read_only: true`, or `--read-only`
on one command) these are refused:

- every other operation: upserts, updates, deletes (by id, by filter or `deleteAll`), namespace,
  index and import operations, and any other spelling (`Delete`, `QUERY`);
- a repeated key such as two `op`s, a non-string `op`, trailing data, and any text that is not
  exactly one JSON object;
- a member the operation does not take (`deleteAll`, `setMetadata` or `vectors` on a read).

Editing is refused on read-only connections. Browse filters run in tobira and similarity filters
are metadata filters, so neither can write.

tobira never calls Pinecone's inference API: there is no operation for embedding, reranking or
text search on an index with integrated embedding. A text search always uses your own
`ai.embedder`.

Where you can, also use an API key whose role cannot write, so Pinecone refuses writes too.

## Limits

| Limit | Value |
| --- | --- |
| Indexes listed | 10,000 |
| `topK` and similarity search | 1000 |
| `fetch` | 1000 ids |
| Browse page | 100 records by default, at most 1000 |
| Records read for a filtered page | 10,000 |
| Sorted browsing | 10,000 records |

## Compatibility

Tested with Pinecone Local 0.7, an in-memory emulator. Not yet tested against the Pinecone service.
Known differences from the service:

- the emulator accepts any API key, so wrong-key errors have not been tested;
- it is always consistent, while the serverless service is eventually consistent: a record you just
  wrote can take a moment to appear in browsing, a fetch or a search, and the duplicate-id check
  can miss it;
- it never reports an index's metric, so set `metric=`;
- read units, rate limits, pod-based indexes and sparse indexes have not been tested;
- the less common metadata filter operators have not been tested.

## Troubleshooting

- **A similarity search says the metric is unknown**: add `metric=` (`cosine`, `euclidean` or
  `dotproduct`) to the DSN.
- **"text similarity needs ai.embedder"**: set `ai.embedder` in the [AI settings](/docs/ai/), or
  search by vector.
- **`tls=false` is refused**: Pinecone's cloud is always TLS. For a self-hosted host that is not
  this machine, add `allow_plaintext=1`.
- **Sorting is not supported for a namespace**: it has more than 10,000 records. Browse unsorted,
  or run a `query` with a filter.
- **A key scoped to one index cannot list indexes**: name the index with `host=` and `index=`.
- **A record you just wrote is missing**: the serverless service is eventually consistent. Wait a
  moment and reload.
