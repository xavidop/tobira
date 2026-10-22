---
layout: ../../../../layouts/DocsLayout.astro
title: "Firestore: queries and types"
---

# Firestore: queries and types

Firestore has no query language, so a tobira query against Firestore is one JSON object that
describes a read. This page lists its keys, the literals for values JSON lacks, and how Firestore
values show in tobira. For connecting, see [Cloud Firestore](/docs/drivers/firestore/).

## Examples

```json
{"collection":"orders","where":[["status","==","paid"],["total",">=",100]],"orderBy":[["total","desc"]],"limit":20}
{"collection":"users/ada/orders","select":["total","item.sku"]}
{"collectionGroup":"orders","where":[["user","==","ada"]]}
{"collection":"orders","where":[["status","==","paid"]],"count":true}
{"document":"users/ada"}
```

```sh
tobira query app '{"collection":"orders","orderBy":[["total","desc"]],"limit":5}'
```

## Request keys

| Key | Value |
| --- | --- |
| `collection` | a collection path: `orders`, or a subcollection `users/ada/orders` |
| `collectionGroup` | a collection id: every collection with that id, at any depth |
| `document` | a document path; it takes no other key |
| `where` | a list of `[field, op, value]`; `op` is `==`, `!=`, `<`, `<=`, `>`, `>=`, `array-contains`, `array-contains-any`, `in` or `not-in` |
| `orderBy` | a list of fields, or `[field, "asc"\|"desc"]` pairs |
| `limit`, `offset` | a positive and a non-negative integer |
| `select` | a list of field paths: the documents hold only those fields |
| `count` | `true` returns one row, `count` |

- Exactly one of `collection`, `collectionGroup` and `document` is required.
- A `document` request for a missing document is "not found".
- Query parameters are not supported: put the values in the request.
- A query that needs a composite index fails with Firestore's message, which includes a link that
  creates the index.

## Field paths

- Field paths are dotted: `address.city`.
- Backquote a segment whose name holds dots or other characters: ``"`a.b`.c"`` is field `c`
  inside the field named `a.b`. Inside backquotes, a backslash escapes the next character.
- `_id` (or `__name__`) is the document id. An `_id` filter takes ids of the queried collection,
  or document paths in a collection group.
- `_id_field` is a stored field named `_id` (see [below](#fields-named-_id)).

## Values

Values are JSON: a whole number is an integer, any other number a double. One-key objects spell
what JSON lacks:

| Literal | Firestore value |
| --- | --- |
| `{"$timestamp":"2026-01-02T03:04:05Z"}` | timestamp (RFC 3339) |
| `{"$bytes":"AQI="}` | bytes (standard base64) |
| `{"$ref":"users/ada"}` | reference |
| `{"$geopoint":[51.5,-0.12]}` | geopoint |
| `{"$vector":[0.1,0.2]}` | vector |
| `{"$double":"NaN"}` | a double: also `Infinity`, `-Infinity`, or a whole number stored as a double |

Any other `$` key is a syntax error.

```json
{"collection":"events","where":[["at",">=",{"$timestamp":"2026-01-01T00:00:00Z"}]]}
{"collection":"orders","where":[["user","==",{"$ref":"users/ada"}]]}
```

## Result rows

- Each row is a document: `_id` (the key) first, then `_path` for collection-group results, then
  the fields.
- Documents differ in fields, so the columns are the union of the fields of the first 1000
  documents (or 32 MiB). Fields that only later documents hold are left out, with a warning that
  says how many. Add a `select` to see them.
- Rows from a `collection` or `document` request can be edited in place.

## Types

| Firestore | tobira |
| --- | --- |
| null, boolean, string, bytes | NULL, bool, string, bytes |
| integer, double | int, float |
| timestamp | time (UTC, microseconds) |
| reference | string: the path relative to the database (`users/ada`) |
| geopoint | object `{latitude, longitude}` |
| array, map | list, object (keys sorted, as Firestore orders them) |
| vector | vector (float32) |
| MongoDB-compatible int32, decimal128 | int, decimal; the other MongoDB-compatible types are unknown |

- Firestore stores vectors as doubles; tobira reads them as float32, and an edited vector is
  written back as those float32 values.
- An edit writes back only what it changed, so an unchanged reference, geopoint or vector stays as
  it is. A reference or geopoint you type is written as a string or a map.
- When you write, a decimal becomes a double (a value that does not fit is refused), JSON is
  stored in its parsed form (objects as maps), a list as an array and an object as a map.
- A document the client library cannot decode keeps its `_id`, without fields, and the result
  warns.

### Fields named `_id`

A stored field named `_id` would hide behind the document-id column, so it is shown as
`_id_field`, and the result says how many documents hold one. Filters, sorts, inserts and edits on
`_id_field` read and write the stored `_id`. A document holding both `_id` and `_id_field` fields
shows only the first, with a warning. In collection-group results, a stored field named `_path` is
hidden by the path column.
