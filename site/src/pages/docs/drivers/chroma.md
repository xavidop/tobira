---
layout: ../../../layouts/DocsLayout.astro
title: Chroma
---

# Chroma

The `chroma` driver connects tobira to Chroma, the open-source vector database, over its v2 REST
API. It is compiled into the `tobira` binary. You query with JSON in the shape of Chroma's own get
and query calls, browse and edit records, and run similarity search by vector or by text.

## Quick start

```sh
tobira query "chroma://localhost:8000" '{"op":"count","collection":"docs"}' --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: rag
    driver: chroma
    dsn: chroma://api.trychroma.com?tenant=acme&database=prod
    password: env:CHROMA_TOKEN
    read_only: true
```

## Connecting

```text
chroma://localhost:8000
chroma://localhost:8000?tenant=acme&database=prod
chroma://chroma.example.com:8000?tls=false&allow_plaintext=1
chroma://api.trychroma.com?tenant=…&database=…
```

| Parameter | Meaning |
| --- | --- |
| `tenant`, `database` | the tenant and database; see below when left out |
| `tls=false` | plain HTTP |
| `allow_plaintext=1` | allow `tls=false` to a host that is not this machine |
| `auth_header=authorization` | send the token as `Authorization: Bearer …` instead of `x-chroma-token` |
| `token` | the token; works, but use the password instead |

- The port defaults to 8000 over HTTP and 443 over HTTPS. A path and a user name are refused.
- The connection's `options:` may set `tenant`, `database`, `tls`, `allow_plaintext` and
  `auth_header`. Any other parameter or option is refused.
- **Token.** Put it in the connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`
  (see [Passwords and secrets](/docs/secrets/)); it wins over the DSN's password position and
  `token=`. It travels as `x-chroma-token`, the header of Chroma Cloud and of Chroma's token
  provider. Tokens never appear in errors.
- **Tenant and database.** Without `tenant=` and `database=`, tobira asks the server who you are
  and uses its tenant, and its database when it lists exactly one; otherwise Chroma's
  `default_tenant` and `default_database`.
- Open reads the database once, so a bad address, token or database fails there.

**TLS** is on unless the host is this machine (`localhost` or a loopback address) or you set
`tls=false`. Plain HTTP to another host also needs `allow_plaintext=1`; a token sent that way
writes a warning to the `--debug` log. Chroma Cloud (`trychroma.com` and its subdomains) refuses
plain HTTP even with it. tobira never follows an HTTP redirect, so the token cannot be forwarded
to another host: put the final address in the DSN.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, collections, describe | yes; namespaces are the tenant's databases; collections are Chroma collections |
| Browse | yes; Chroma's order, or sorted by tobira (up to 10,000 records); filters run on the server |
| Similarity search | yes; the collection's distance; by vector, or by text with an [embedder](/docs/ai/) |
| Editing | yes; insert, update and delete of single records by id |
| Explain, transactions, completion, native export, watching | no |

## Queries

A query is one JSON object:

```json
{"collection": "docs", "where": {"year": {"$gte": 2020}}, "whereDocument": {"$contains": "vector"}, "limit": 20, "offset": 0, "include": ["documents", "metadatas", "embeddings"]}
{"collection": "docs", "queryEmbeddings": [[0.1, 0.2, 0.3]], "nResults": 5, "where": {"lang": "en"}}
{"collection": "docs", "queryTexts": ["how do I search vectors?"], "nResults": 5}
{"op": "count", "collection": "docs"}
```

| op | Members | Notes |
| --- | --- | --- |
| `get` | `ids`, `where`, `whereDocument`, `limit`, `offset`, `include` | without a limit, the whole collection streams 300 at a time |
| `query` | `queryEmbeddings` or `queryTexts` (at most 100), `nResults`, `ids`, `where`, `whereDocument`, `include` | `nResults` defaults to 10, at most 1000 |
| `count` | none | one row |

- Without `op`, a request with `queryEmbeddings` or `queryTexts` is a `query`, anything else a
  `get`.
- `include` takes `documents`, `metadatas`, `embeddings`, `uris` and, for a query, `distances`.
  Documents and metadatas (and distances) come back by default.
- `where` and `whereDocument` are Chroma's own filter objects, sent as they are.
- **`queryTexts` are embedded by tobira's `ai.embedder`**, never by Chroma or by an embedding
  function stored in the collection's configuration. Without an embedder they are refused with
  "text similarity needs ai.embedder".
- A query's `_distance` is Chroma's own: the squared L2 for an `l2` collection, 1 − cosine for
  `cosine`, 1 − inner product for `ip`. With several query embeddings, `_query` says which one a
  row answers.
- Chroma's snake_case spellings (`where_document`, `n_results`, `query_embeddings`,
  `query_texts`) work too; giving both spellings is refused.
- Queries take no parameters. The namespace picks the database.

## Records as rows

A record is a row: `id`, `document`, `uri` (when included or present), `embedding`, then one
column per metadata key, in name order.

- **Ids** are strings in Chroma. An id that is a plain integer (`7`, `-3`; not `07`) shows as a
  number and is written back as the same text. When any id in a result is not such an integer,
  the whole id column is text.
- **Metadata** holds booleans, integers, floats, strings and arrays of one of them. Chroma keeps
  integers and floats apart, and so does tobira.
- A metadata key named like a record column (`id`, `document`, `uri`, `embedding`, `_distance`,
  `_query`), or starting with `meta_`, shows as `meta_<key>`, with a warning. Filters and edits on
  that column act on the metadata key.
- Keys first seen after the first page of a streamed result are named in a warning.

## Browsing

- **Databases** are the tenant's databases; the connection's database is the default. A token that
  may not list databases sees only its own. Collections are listed, at most 10,000.
- **Unsorted** pages follow Chroma's own record order, with an offset cursor. A record written or
  deleted between pages shifts the offsets.
- **Sorted** pages: Chroma cannot order a get, so tobira reads the matching records (at most
  10,000; filter first for more), sorts them with NULLs last and ties by id, then fetches the
  page's embeddings. Sorting by `embedding` is not supported.
- **Filters** run on the server: `id` equals and in (at most 1,000 ids), `document` contains and
  starts with, and metadata equals, not equals, comparisons and in. Contains, starts with and the
  null checks on metadata are not supported: Chroma has no substring match on metadata and no test
  for a missing key.
- A **raw filter** is a Chroma `where` object, such as `{"tags": {"$contains": "x"}}` for an
  array, combined with the other filters.

## Editing

Records are edited one at a time, by `id`.

- **Insert** needs an `embedding` (a list of numbers): Chroma's API never embeds, and tobira does
  not embed on a write. Without an id, tobira generates a UUID. `document` and `uri` are strings;
  every other column is a metadata key.
- **Update** merges metadata (NULL removes a key) and can change the embedding, document and uri.
  The id cannot change. A NULL document, uri or embedding is refused, because Chroma would keep the
  old one.
- **Delete** removes one record.
- **Values**: integers, floats, booleans and strings as they are; decimals as floats when a float
  holds them exactly; times as RFC 3339 text; JSON as its text; a non-empty list of one scalar type
  as an array. Empty lists, objects and bytes are refused.
- Chroma's own add, update and delete never report a clash or a missing record, so tobira checks
  first. A concurrent writer can still slip in between the check and the write.

## Similarity search

Press `S` on a row, use `:similar`, or ask the AI assistant.

- Chroma fixes the distance per collection (l2, cosine or ip). The default metric is the
  collection's; another metric is refused.
- A text search needs `ai.embedder` in the [AI settings](/docs/ai/). The vector must match the
  collection's dimension.
- Results have a `_distance` column, nearest first: the Euclidean distance (the root of Chroma's
  squared L2), 1 − cosine similarity, or the negated inner product.
- `k` defaults to 10, at most 1000.

## Read-only guard

Queries never write, on any connection: only `get`, `query` and `count` run, and the request is
parsed before tobira sends anything. On a read-only connection (`read_only: true`, or `--read-only`
on one command) these are refused:

- every other operation: `add`, `upsert`, `update`, `delete`, `fork`, collection, database and
  tenant management, function attachment, and Chroma's `search` API, which can embed text on the
  server;
- unknown members (such as `embeddings` in a get), a member the operation does not take, a member
  given twice (directly or through its snake_case spelling), duplicate keys, trailing text and
  wrong types.

Editing is refused on read-only connections. tobira never calls a Chroma endpoint that embeds:
query texts go only to your own `ai.embedder`, even when the collection's configuration names a
hosted embedding function such as OpenAI's.

## Limits

| Limit | Value |
| --- | --- |
| Collections listed | 10,000 |
| Query texts or embeddings per query | 100 |
| `nResults` and similarity search | 1000 |
| Sorted browsing | 10,000 records |
| `id` filter | 1,000 ids |
| Reply size | 64 MiB |

## Compatibility

Tested with Chroma 1.5. Token authentication is tested against a server that checks
`x-chroma-token` the way Chroma Cloud does. Not yet tested: Chroma Cloud, `spann` collections, and
tenants other than the default.

## Troubleshooting

- **"text similarity needs ai.embedder"**: set `ai.embedder` in the [AI settings](/docs/ai/), or
  search with `queryEmbeddings`.
- **A redirect error**: the server redirected, which tobira never follows. Put the final address
  in the DSN.
- **`tls=false` is refused**: add `allow_plaintext=1` for a host that is not this machine. Chroma
  Cloud needs TLS.
- **401 behind a proxy that expects a bearer token**: add `auth_header=authorization`.
- **A reply is too large**: ask for fewer records, or leave `embeddings` out of `include`.
- **Sorting is not supported**: more than 10,000 records match. Filter first.
