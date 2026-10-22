---
layout: ../../../layouts/DocsLayout.astro
title: Azure Cosmos DB
---

# Azure Cosmos DB

The `cosmos` driver connects tobira to Azure Cosmos DB for NoSQL through the Azure SDK for Go. It
is compiled into the `tobira` binary. You query with Cosmos SQL `SELECT`s, browse and edit
containers, run vector search and follow the change feed. An account with the API for MongoDB
works through the [MongoDB driver](/docs/drivers/mongodb/) instead.

## Quick start

```yaml
connections:
  - name: shop
    driver: cosmos
    dsn: cosmos://myaccount.documents.azure.com?database=shop
    password: keyring:shop
    read_only: true
```

```sh
tobira connections set-password shop
tobira query shop "SELECT * FROM orders o WHERE o.status = 'paid'"
```

## Connecting

```text
cosmos://myaccount.documents.azure.com?database=shop
cosmos://myaccount.documents.azure.com:443?database=shop
cosmos://localhost:8081?database=shop&tls=false
```

| Parameter | Meaning |
| --- | --- |
| host | the account endpoint; port 443 by default (8081 with `tls=false`) |
| `database` | the default database; without it, pick a database for every call |
| `tls=false` | plain HTTP, as the emulator uses |
| `allow_plaintext=1` | allow `tls=false` to a host that is not this machine |
| `key`, `accountkey` | the account key; works, but use the password instead |

- **The account key** goes in the connection's `password:` reference or
  `TOBIRA_CONN_<NAME>_PASSWORD` (see [Passwords and secrets](/docs/secrets/)). It wins over a
  `key=` in the DSN, and tobira masks `key=` wherever it shows a DSN. No error shows the key.
- Microsoft Entra ID sign-in and resource tokens are not supported.
- Unknown parameters, credentials before the host, a path and the connection's `options:` are
  refused.
- Open fails on a wrong endpoint, a wrong key, a blocked client or a missing database.

**TLS.** Connections use HTTPS by default. `tls=false` is allowed on this machine (`localhost` or a
loopback address), and on another host only with `allow_plaintext=1`, since the signed requests
and the data would cross the network unencrypted. Azure's own endpoints
(`*.documents.azure.com`, `*.documents.azure.cn`, `*.documents.azure.us`) refuse `tls=false` even
with it. `allow_plaintext` alone never turns TLS off.

tobira never follows an HTTP redirect, and sends signed requests only to the DSN's host or, for an
Azure account, to another of the account's regional endpoints in the same Azure zone.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, containers, describe | yes; a query is one Cosmos SQL `SELECT`; describe reads the container's definition and samples 100 items |
| Browse | yes; filters become `WHERE`, sorting `ORDER BY`, paged by continuation tokens |
| Editing | yes; insert, update (replace with an ETag match) and delete of one item, by id plus partition key |
| Vector search | yes; `VectorDistance` on a container with a vector policy (L2, cosine, inner product; text with an embedder) |
| Completion | yes; keywords, functions, containers after `FROM`, fields after `alias.`; cached 60 s, refreshed by `:reload` |
| Watching | yes; the change feed, polled every second |
| Explain, transactions, native export | no (`:export` and `--output` use tobira's own writers) |

Cosmos DB shows no query plan, and its transactional batch takes every operation in one call, so
there is no explain and no `BEGIN`/`COMMIT`.

## Queries

A query is one Cosmos SQL `SELECT`. The container is the name after the top-level `FROM`:

```sql
SELECT * FROM orders o WHERE o.status = 'paid' ORDER BY o.total DESC
SELECT VALUE COUNT(1) FROM orders
SELECT o.id, l.sku FROM orders o JOIN l IN o.lines
SELECT t FROM t IN orders.tags
```

To read one logical partition, or to pass named parameters, send a JSON envelope with exactly
these keys (`container` and `query` are required):

```json
{"container": "orders",
 "query": "SELECT * FROM c WHERE c.total > @min",
 "partitionKey": "acme",
 "parameters": {"@min": 10}}
```

- `partitionKey` is one value, or an array for a hierarchical key.
- Positional query parameters bind as `@p1`, `@p2` ….
- When every item is an object, the columns are `id`, then the fields of the first 1000 items,
  then the system properties (`_rid`, `_self`, `_etag`, `_attachments`, `_ts`). Fields only later
  items hold are left out, with a warning. A `SELECT VALUE` result has one `value` column.
- Rows of a `SELECT *` can be edited in the grid.
- The status line shows the request charge in RU.
- On a container with several physical partitions, cross-partition `ORDER BY`, `GROUP BY`,
  `DISTINCT`, aggregates and `OFFSET … LIMIT` need a `partitionKey`: the Go SDK has no client-side
  query engine. Sorting on several fields needs a composite index.

## Browsing

- **Databases** are the namespaces; the DSN's database is the default.
- **Containers** are listed by name, at most 10 000, each with its partition key.
- **Describe** reads the container's definition and samples up to 100 items: `id` first, nested
  fields, mixed types as unions such as `number|string`, and the system properties. The key is the
  partition key's fields plus `id`. Indexes list unique keys, composite indexes and vector
  indexes. Container properties are cached for 60 seconds; `:reload` drops them.
- **Filters**: `=`, `!=`, `<`, `<=`, `>`, `>=` (`= null` matches a missing or null field), `in`,
  `contains` and `prefix` on strings, `is null` (missing or null) and `is not null`. A field name
  with dots is a nested path (`ship.city`).
- A **raw filter** is a Cosmos SQL condition on the alias `c`, such as `ARRAY_LENGTH(c.lines) > 2`.
  It must keep its parentheses balanced and hold no `;` or `--` comment.
- **Paging** follows continuation tokens. A page is 100 items by default, at most 1000.

## Editing

Every edit addresses one item by its `id` and partition key.

- **Insert** never replaces an item. Its `id` must be a string; without one, tobira picks a UUID.
  The item must hold its partition key field: an item without one could not be edited or deleted
  afterwards, so it is refused. An existing id in the partition is a constraint error.
- **Update** replaces the item only if it still has the ETag tobira read, so a change made in
  between fails instead of being overwritten. The id, the partition key fields and the system
  properties cannot change: insert a copy and delete the original.
- **Delete** of a missing item reports "not found".

## Vector search

`S` and `:similar` work on a container with a vector embedding policy.

- The field is the one you name, or the policy's only vector path.
- tobira ranks items with `VectorDistance`, using the policy's distance function, or the metric
  you pick (`l2`, `cosine`, `ip`) as an exact search.
- `_distance` is the Euclidean distance, 1 − cosine similarity or the negated dot product, nearest
  first.
- A text query needs an [embedder](/docs/ai/) (`ai.embedder`), which turns the text into a vector.
- Filters work as in browsing.

## Watching

`:watch` (or `W`) on a container, or `tobira watch <connection> <container>`, follows its change
feed from now, polled every second. Every created or replaced item shows as an `upsert` with its
key and body: the feed does not tell creates and replaces apart. Deletes are not in the feed's
latest-version mode, so they do not show.

## Read-only guard

Cosmos SQL cannot write, and tobira still checks every statement on a read-only connection
(`read_only: true`, or `--read-only` on one command). It refuses anything it cannot recognise as a read. A statement passes
only when:

- it is one `SELECT`, with no `;`;
- every string and `--` comment is terminated, and no character falls outside the language;
- it calls no user-defined function (`udf.…`), so user JavaScript never runs;
- an envelope holds only `container`, `query`, `partitionKey` and `parameters`, each once.

Anything else is refused before it is sent. **Stored procedures and triggers are never run**:
tobira has no way to call one and never attaches a trigger to a write. A raw browse filter on a
read-only connection cannot call a UDF either. Editing is refused on a read-only connection.

Give read-only connections the account's **read-only key**: the service then refuses every write
by itself.

## Types

| Cosmos DB (JSON) | tobira |
| --- | --- |
| string | string |
| number without a fraction or exponent | int (decimal beyond 64 bits) |
| other number | float |
| true, false | bool |
| null | NULL |
| array | list |
| object | object, key order kept |

Cosmos DB stores numbers as doubles, so a decimal written from tobira keeps about 15 significant
digits. Times are written as RFC 3339 text, bytes as base64 and vectors as arrays. NaN and
infinities are refused.

## Limits

| Limit | Value |
| --- | --- |
| Containers listed | 10 000 |
| Browse page | 100 items by default, at most 1000 |
| Columns of a query result | the fields of the first 1000 items or 32 MiB |
| Describe sample | 100 items, 5 seconds, 32 MiB |

## Compatibility

Tested with the Azure Cosmos DB Linux emulator (the `vnext-preview` build) in HTTP mode. Not yet
tested against an Azure account. Known differences from the service:

- the emulator accepts any key, so wrong-key errors have not been tested;
- it does not enforce unique key policies, and accepts a numeric `id`;
- it does not report a container's vector policy, so tobira finds the vector field from the vector
  index and searches exactly;
- it has one physical partition, so the cross-partition limits above never show;
- it has no stored procedures, triggers or UDFs;
- it charges a nominal 1 RU and never throttles.

Watching uses the change feed's latest-version mode, which does not include deletes.

## Troubleshooting

- **"tls=false to a host that is not this machine also needs allow_plaintext=1"**: add it on a
  trusted network, or use TLS.
- **"tls=false is refused for Azure Cosmos DB endpoints"**: Azure accounts need TLS. Remove
  `tls=false`.
- **"throttled: the request rate exceeds the provisioned throughput"**: retry later, or raise the
  container's RU/s.
- **A cross-partition `ORDER BY`, `GROUP BY` or aggregate fails**: send the query in an envelope
  with a `partitionKey`.
- **A sort on several fields fails**: add a composite index for those fields to the container's
  indexing policy.
- **An update fails with a conflict**: the item changed after tobira read it. Reload and edit again.
