---
layout: ../../../layouts/DocsLayout.astro
title: Weaviate
---

# Weaviate

The `weaviate` driver connects tobira to Weaviate, the vector database, over its REST and GraphQL
APIs with the official Go client. It is compiled into the `tobira` binary. You query with JSON
requests or raw GraphQL reads, browse and edit objects, and run similarity search by vector or by
text.

## Quick start

```sh
tobira query "weaviate://localhost:8080" '{"collection":"Article","limit":5}' --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: articles
    driver: weaviate
    dsn: weaviate://xyz.weaviate.cloud
    password: env:WEAVIATE_API_KEY
    read_only: true
```

## Connecting

```text
weaviate://localhost:8080
weaviate://weaviate.example.com:8080?scheme=http&allow_plaintext=1
weaviate://xyz.weaviate.cloud
```

| Parameter | Meaning |
| --- | --- |
| `scheme` | `http` or `https`; `http` by default on this machine, `https` elsewhere |
| `allow_plaintext=1` | allow `scheme=http` to a host that is not this machine |
| `api_key`, `apikey` | the API key; works, but use the password instead |

- The port defaults to 8080 for `http` and 443 for `https`.
- `scheme` and `allow_plaintext` can also be set in the connection's `options:`.
- **API key.** Put it in the connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`
  (see [Passwords and secrets](/docs/secrets/)); it wins over the DSN. A key in the DSN
  (`weaviate://:KEY@host` or `?api_key=KEY`) works too and is masked wherever tobira prints the
  DSN. It is sent as a Bearer token. Keys never appear in errors.
- A user name, a path, a key given twice and any other parameter are refused.
- Open checks the server once. Servers older than 1.14 are refused.

**TLS.** `scheme=http` to a host that is not this machine (`localhost` or a loopback address) also
needs `allow_plaintext=1`, and `allow_plaintext` alone keeps HTTPS. A key sent in clear to another
host writes a warning to the `--debug` log. Weaviate Cloud (`*.weaviate.cloud`,
`*.weaviate.network`) is always HTTPS, even with `allow_plaintext=1`. tobira never follows an HTTP
redirect, so the key cannot be forwarded to another host.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, collections, describe | yes; one namespace, `default`; collections are Weaviate collections |
| Browse | yes; id order, or filtered and sorted by properties |
| Similarity search | yes; by vector, or by text with an [embedder](/docs/ai/) |
| Editing | yes; insert, update and delete of single objects by id |
| Explain, transactions, completion, native export, watching | no |

## Queries

A query is a JSON request or one raw GraphQL read.

### JSON requests

```json
{"collection":"Article","fields":["title","n"],"where":{"path":["n"],"operator":"GreaterThan","valueInt":2},"limit":10}
{"collection":"Article","sort":[{"path":["n"],"order":"desc"}],"limit":10,"offset":20}
{"collection":"Article","nearVector":{"vector":[0.1,0.2,0.3],"targetVectors":["title_vec"]},"limit":5}
{"collection":"Article","bm25":{"query":"database","properties":["title"]}}
```

| Key | Meaning |
| --- | --- |
| `collection` | required |
| `fields` | the properties to return; all but cross-references by default |
| `where`, `sort` | Weaviate's filter and sort, written as JSON |
| `limit`, `offset` | page size and offset |
| `after` | an id to resume after (not with a filter, sort, offset or search) |
| `vectors` | `true` also returns the vector columns |
| `nearVector`, `nearObject`, `nearText`, `bm25`, `hybrid` | at most one search |

- Values are written as in Weaviate's GraphQL arguments.
- A search returns 10 results unless `limit` says otherwise. Results add `_distance` (vector
  search, Weaviate's own distance) or `_score` (bm25, hybrid).
- Without a filter, sort or offset, the whole collection streams in id order, 500 at a time.
- With one, rows come in offset pages, and Weaviate returns at most 10,000 results that way; past
  that the stream stops with a warning.
- Unknown keys, duplicate keys, a second search and anything after the object are refused.

### Raw GraphQL

Exactly one `{ Get { … } }` or `{ Aggregate { … } }` on one collection, optionally written
`query Name { … }`:

```graphql
{ Get { Article(where: {path: ["n"], operator: Equal, valueInt: 3}) { title _additional { id distance } } } }
{ Aggregate { Article { meta { count } } } }
```

- Mutations, subscriptions, several operations, fragment definitions and spreads, variables,
  directives, `Explore` and introspection are refused before anything is sent. Inline
  `... on Class` works, for cross-references.
- Get columns follow the selection: aliases are honoured, properties are typed by the schema, and
  `_additional` members become columns (`id`, `_distance`, `_score`, `_creationTimeUnix` …).
- Aggregate rows have one column per selected field.

Queries never write, on any connection: edit objects in the grid. Query parameters are refused.

## Objects as rows

Each object is a row: `id`, the properties in schema order, then one column per vector (`vector`
for the default vector, otherwise the vector's name). A vector whose name clashes with a property
gets a `_vector` suffix.

**Ids.** Weaviate names objects by UUID. A UUID of the form `00000000-0000-0000-XXXX-XXXXXXXXXXXX`
shows as the integer its last 16 hex digits spell, and an integer id you type is written that way,
so integer keys survive a round trip. Other UUIDs show as text.

| Weaviate | tobira |
| --- | --- |
| `text`, `string`, `uuid` | string |
| `int` | int |
| `number` | float |
| `boolean` | bool |
| `date` | time |
| `blob` | bytes |
| `object`, `geoCoordinates`, `phoneNumber` | object |
| `X[]` | list |
| cross-reference | not selected; query it with raw GraphQL |

Weaviate has no decimal or JSON type. A value that does not fit its declared type is shown as it
is, not dropped.

## Browsing

- There is one namespace, `default`. Collections are listed by name, at most 10,000. Weaviate
  capitalises a collection's first letter; tobira finds it by either spelling.
- **Describe** shows the properties (with nested members), the vectors with their size from a
  sample, the vector indexes (`hnsw cosine`, `flat l2-squared`), the BM25 and range indexes, the
  object count and the class JSON.
- **Paging** follows object ids. Filtering or sorting switches to offset pages, with the id
  breaking ties; at most 10,000 results can be paged that way. NULLs sort first ascending. The id
  and scalar properties sort; lists, objects and vectors do not.
- **Filters**: comparisons on scalars, equals and not equals on the id, `in`, and is null or is
  not null (which need `invertedIndexConfig.indexNullState` on the collection).
- Contains and starts with on text use Weaviate's `Like`. They match the property's words
  (case-insensitively for word tokenization), not the whole string. A value holding `*` or `?` is
  refused, because `Like` has no escape.
- On a list, equals and contains mean "holds the value".
- A **raw filter** is a Weaviate `where` filter in JSON.

## Editing

Objects are edited one at a time, by `id` (an integer or a UUID). Vector columns take a list of
numbers; every other column must be an existing property, since tobira never lets Weaviate's
auto-schema add one.

- **Insert** uses the `id`, or lets Weaviate assign a UUID. An id that is already taken is refused
  rather than overwritten.
- **Update** is a merge: the fields you edit change, the others keep their values, and NULL clears
  a property. The id cannot change.
- **Delete** checks that the object exists first, since Weaviate deletes a missing object without
  complaint.

Cross-references and multi-vectors are shown but not edited.

## Similarity search

Press `S` on a row, use `:similar`, or ask the AI assistant.

- tobira searches one vector with `nearVector`: the one you name, the collection's only vector, or
  else the only one whose distance matches the requested metric. Weaviate fixes a vector's
  distance, so another metric is refused.
- A text search needs `ai.embedder` in the [AI settings](/docs/ai/). tobira embeds the text
  itself, never through a Weaviate module.
- Results have a `_distance` column, nearest first: the true L2 distance for `l2-squared` vectors,
  1 − cosine for cosine and the negated product for dot.
- `k` defaults to 10, at most 1000. Multi-vectors are not searched.

## Read-only guard

Queries never write. But some Weaviate reads call **modules**: `nearText` and `hybrid` send the
query text to the collection's vectorizer, and `generate`, `ask` or `rerank` call a model. With a
hosted module (OpenAI, Cohere, Anthropic …) that sends your data out and costs money. On a
read-only connection (`read_only: true`, or `--read-only` on one command) tobira refuses:

- **`nearText` and `hybrid`**, unless every vectorizer of the collection runs locally (`none`,
  `text2vec-transformers`, `text2vec-contextionary`, `text2vec-model2vec`, `text2vec-gpt4all`,
  `multi2vec-clip`, `multi2vec-bind`, `img2vec-neural`, `ref2vec-centroid`) and none of them is
  configured with its own inference URL (which could point anywhere). An unknown vectorizer, or a
  collection that declares none, counts as remote. A `hybrid` with an explicit vector is refused
  too.
- **Search arguments** outside the plain reads (`where`, `limit`, `offset`, `after`, `sort`,
  `nearVector`, `nearObject`, `bm25`, `groupBy`, `group`, `autocut`, `tenant`,
  `consistencyLevel`, `objectLimit`): `ask`, `nearImage`, `nearAudio`, `nearMedia` and the rest.
- **`_additional` fields** outside the plain ones (`id`, `vector`, `vectors`, `distance`,
  `certainty`, `score`, `explainScore`, `creationTimeUnix`, `lastUpdateTimeUnix`, `isConsistent`,
  `group`): `generate`, `rerank`, `answer`, `summary`, `spellCheck` and the rest, at any depth.
- Any other field with arguments, except `topOccurrences` in an Aggregate.

Search by vector instead: similarity search embeds text with tobira's own `ai.embedder`. On a
writable connection these queries run. Editing is refused on read-only connections.

## Limits

| Limit | Value |
| --- | --- |
| Collections listed | 10,000 |
| Filtered or sorted results | 10,000 (Weaviate's `QUERY_MAXIMUM_RESULTS`) |
| Similarity search | 1000 neighbours |
| Response size | 64 MiB |

Multi-tenant collections are listed and described, but tobira sends no tenant, so reading them
fails with Weaviate's error.

## Compatibility

Tested with Weaviate 1.40, with anonymous access and with API keys, including the read-only
refusals on a collection with a hosted vectorizer. Not yet tested: Weaviate Cloud over HTTPS,
multi-tenancy, OIDC sign-in, and searches through a real hosted vectorizer.

## Troubleshooting

- **"… calls the … vectorizer, a remote service; search by vector instead"**: the collection uses
  a hosted vectorizer and the connection is read-only. Use similarity search with `ai.embedder`,
  or a writable connection.
- **`scheme=http` is refused**: add `allow_plaintext=1` for a host that is not this machine.
  Weaviate Cloud needs HTTPS.
- **Is null filters fail**: turn on `invertedIndexConfig.indexNullState` for the collection.
- **Paging stops at 10,000 results**: narrow the filter, or browse unfiltered, which follows ids
  without that limit.
- **Reading a multi-tenant collection fails**: tobira does not send a tenant. Use a raw GraphQL
  query with a `tenant` argument.
- **An edit fails with "not found" for a field**: the property does not exist in the schema. Add
  it to the collection first.
