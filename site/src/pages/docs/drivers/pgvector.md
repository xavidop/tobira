---
layout: ../../../layouts/DocsLayout.astro
title: pgvector
---

# pgvector

[pgvector](https://github.com/pgvector/pgvector) adds a `vector` column type and nearest-neighbour
search to PostgreSQL. In tobira it is part of the [PostgreSQL driver](/docs/drivers/postgres/), not a
separate driver: connect with a normal `postgres://` DSN, and similarity search turns on when the
database has the extension.

```sql
CREATE EXTENSION vector;
CREATE TABLE items (id bigint PRIMARY KEY, embedding vector(768), title text);
```

## Quick start

```yaml
connections:
  - name: local-pg
    dsn: postgres://app@localhost:5432/app
    password: env:PG_PASSWORD
```

```sh
tobira query local-pg "select id, title from items order by embedding <-> '[0.1, 0.2, 0.3]' limit 5"
```

In the terminal UI, open `items`, then press `S` on a row to see its nearest neighbours, or run
`:similar <text>` to search by text with your [embedder](#searching-by-text).

## When it is available

tobira checks for the extension once, when it connects. If the database has it, the connection
offers vector search; if not, everything else works as usual and vector search reports that it is
not supported. After you run `CREATE EXTENSION vector`, reconnect so tobira sees it.

The extension may live in any schema, including one outside your `search_path`.

## Vector values

A `vector` column is shown as a list of numbers, and values you edit or pass as query parameters
are sent back as vectors, so a `vector(4)` column round-trips exactly. tobira reads `vector`
columns only; `halfvec`, `sparsevec` and `bit` columns are shown as their text form.

## Similarity search

A search names a table or view, a query vector (or text, see below) and optionally a column, a
metric, a number of results and a filter. tobira returns the rows nearest first, with an extra
`_distance` column:

| Metric | Operator | `_distance` |
| --- | --- | --- |
| L2 | `<->` | Euclidean distance |
| cosine | `<=>` | 1 − cosine similarity |
| inner product | `<#>` | the negated inner product |

Smaller is always nearer, whatever the metric.

- **Column.** The column you name, or the table's only `vector` column. A table with several
  vector columns needs the column named.
- **Metric.** When you do not choose one, tobira uses the metric of an HNSW or IVFFlat index on the
  column, so the index can serve the search. Without such an index it uses cosine.
- **Number of results.** 10 by default, at most 1000.
- **Filter.** The same filters as browsing a table. Rows whose vector is NULL are left out.
- **Distance column.** If the table already has a column named `_distance`, the distance is
  `_distance_2` instead (or the next free `_distance_N`), and the table's own column is kept.
- **Changes to the table.** tobira remembers a table's columns and its vector index for a minute.
  After adding an index or a column, run `:reload` to search with the change at once.
- **Checks.** A query vector whose length differs from the column's size (`vector(768)`), or that
  holds NaN or infinity, is refused before anything runs.

## Searching by text

To search with words instead of a vector, set `embedder` in the [AI configuration](/docs/ai/), for
example `googleai/text-embedding-005` or `ollama/nomic-embed-text`. tobira turns the text into a
vector with that model and then searches as above. The model must produce vectors of the column's
size. Without an embedder, a text search says it needs one; if the embedding service fails (a bad
key, say), you see that error.

## Read-only guard

A similarity search is always a read. It runs inside a read-only transaction that is rolled back,
on read-write connections too, so a filter that tries to write fails as a read-only violation and
changes nothing.

## Limits

- The extension is detected when tobira connects, not later.
- Only `vector` columns are searched.
- Approximate indexes (HNSW, IVFFlat) return approximate neighbours, as they do in `psql`.

## Compatibility

Tested with pgvector 0.8.7 on PostgreSQL 17. [Greenplum](/docs/drivers/greenplum/) offers the same
search when the extension is installed; CockroachDB and Amazon Redshift do not.
