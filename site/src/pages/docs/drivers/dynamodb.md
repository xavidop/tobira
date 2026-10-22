---
layout: ../../../layouts/DocsLayout.astro
title: Amazon DynamoDB
---

# Amazon DynamoDB

The `dynamodb` driver connects tobira to Amazon DynamoDB through `aws-sdk-go-v2`. It is compiled
into the `tobira` binary. You query with PartiQL, browse tables as scans, edit one item at a time
and follow DynamoDB Streams.

## Quick start

```sh
tobira query "dynamodb://eu-west-1?profile=prod" \
  "SELECT * FROM \"orders\" WHERE customer = 'c-42'" --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: orders
    driver: dynamodb
    dsn: dynamodb://AKIAIOSFODNN7EXAMPLE@eu-west-1
    password: keyring:orders
    read_only: true
```

## Connecting

```text
dynamodb://us-east-1
dynamodb://eu-west-1?profile=prod
dynamodb://AKIAIOSFODNN7EXAMPLE@us-east-1
dynamodb://local@us-east-1?endpoint=http://localhost:8000
dynamodb://
```

- The host is the **region**. Without one, `AWS_REGION` or the profile must supply it.
- Unknown parameters and the connection's `options:` are refused.

| Parameter | Meaning |
| --- | --- |
| `endpoint` | another service endpoint, such as DynamoDB Local (`http://` or `https://`) |
| `profile` | a shared-config profile; cannot be combined with an access key |
| `allow_plaintext` | allow an `http://` endpoint on a host that is not this machine |

**Credentials** come from one of three places:

- an **access key id** as the DSN user, with the secret access key in the connection's password
  (a `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`; see
  [Passwords and secrets](/docs/secrets/));
- a **profile** (`?profile=`);
- the **AWS default chain**: environment variables, shared files, SSO, web identity, container and
  instance roles. Session tokens come only from the chain or a profile.

**TLS.** AWS endpoints are always HTTPS. An `http://` endpoint is allowed on this machine
(`localhost` or a loopback address), and on another host only with `allow_plaintext=1`: requests
are then signed but not encrypted, and tobira writes a warning to the debug log. AWS's own hosts
(`*.amazonaws.com`, `*.amazonaws.com.cn`) refuse an `http://` endpoint even with it. tobira never
follows an HTTP redirect, so signed requests and session tokens cannot be forwarded to another
host.

The connection fails on open when it finds no credentials, cannot reach the endpoint, or AWS
rejects the credentials. The secret access key and session token are masked as `xxxxx` in every
error, and errors never quote the DSN.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, tables, describe | yes; a query is one PartiQL statement; describe reads the keys and indexes and samples 100 items |
| Browse | yes; a scan paged by key; filters become a `FilterExpression`; sorting reads at most 10 000 items |
| Editing | yes; insert, update and delete of one item, by its full primary key |
| Completion | yes; PartiQL keywords and functions, tables and sampled attributes; cached 60 s, refreshed by `:reload` |
| Watching | yes; DynamoDB Streams, on a table with its stream enabled |
| Explain, transactions, native export | no (`:export` and `--output` use tobira's own writers) |

DynamoDB has no query plan and no session transaction, so there is no explain and no
`BEGIN`/`COMMIT`.

## Queries

A query is one PartiQL statement.

```sql
SELECT * FROM "orders" WHERE customer = 'c-42'
SELECT id, status FROM "orders"."by_status" WHERE status = 'paid'
SELECT * FROM "orders" WHERE customer = ? ORDER BY created DESC
UPDATE "orders" SET status = 'shipped' WHERE customer = 'c-42' AND id = 7
```

- One statement per call, never batched or run as a transaction. `?` placeholders take the query's
  parameters, in order.
- A SELECT that lists plain attribute names gets exactly those columns; a missing attribute reads
  as NULL. `SELECT *` gets the key attributes first, then every attribute of the first 1000 items,
  sorted. Attributes that only later items hold are left out, with a warning.
- A row read from a table (or one of its indexes) can be edited in the grid: the key attributes are
  known.
- `ORDER BY` needs an equality on the partition key, as DynamoDB requires. Use browse to sort a
  whole table.
- Writes return no rows and no affected count, because DynamoDB reports none. An `INSERT` of an
  existing key is a constraint error; an `UPDATE` whose `WHERE` matches nothing is a conflict.

## Browsing

- There is one namespace, `default`, labelled with the region.
- **Tables** are listed by name, at most 10 000.
- **Describe** shows the key attributes first (partition key, then sort key), then every attribute
  of a 100-item sample, with nested fields for maps. It also shows the primary key, each global and
  local secondary index, AWS's item count and table size (refreshed by AWS about every six hours),
  the status, billing mode and stream.
- **Filters** become a `FilterExpression`:
  - `=`, `!=`, `<`, `<=`, `>`, `>=` compare values of the same type only (a number never equals a
    string);
  - `in`, at most 100 values;
  - `contains` (a substring, or a member of a set or list) and `prefix` (`begins_with`);
  - `is null` (missing or NULL) and `is not null`.

  A field is one top-level attribute; a dot is part of its name, not a path. `contains` and
  `prefix` on a number key attribute are refused, because DynamoDB never matches them. A raw
  filter is not supported, because DynamoDB expressions cannot hold literal values: write a PartiQL
  query instead.
- **Paging** follows the scan. A page is 100 items by default, at most 10 000, and ends early at
  32 MiB. Scans cost read capacity for every item read, including the ones a filter drops.
- **Sorting** a column reads the whole table into memory (at most 10 000 items and 32 MiB), sorts
  it and pages it. Missing and NULL values sort first ascending.

## Editing

Every edit addresses one item by its full primary key: the partition key, plus the sort key when
the table has one. The other values the grid showed become conditions, so an edit from a stale row
fails instead of overwriting.

- **Insert** never replaces an existing item. It must hold every key attribute.
- **Update** sets the changed attributes. Setting one to NULL stores NULL; removing an attribute is
  not supported. It fails when the item no longer exists or no longer holds the values the grid
  showed.
- **Delete** fails in the same cases.

A key attribute cannot change: delete the item and insert a new one instead.

tobira caches each table's key schema for 60 seconds. If you recreate a table with a different
key, run `:reload` so tobira picks up the new key at once.

## Completion

The editor completes table names after `FROM`, `INTO` and `UPDATE`, and otherwise tables, the
attributes of the statement's table, PartiQL functions (`begins_with`, `contains`,
`attribute_type`, `size` …) and keywords. A name that is not a plain identifier is inserted
quoted.

## Watching

`:watch` on a table, `W` in the TUI, or `tobira watch <connection> <table>` follows its DynamoDB
Stream. Enable the stream first, with view type `NEW_AND_OLD_IMAGES`.

- Events show inserts, updates and deletes with the item's key and new image (the old one for a
  delete).
- Shards are polled once a second, and one item's changes stay in order across shard rollovers.
- Watching starts at the tip of the stream: you see changes made after it starts.
- The watch ends when its tab or the connection closes.

## Read-only guard

DynamoDB has no read-only session, so tobira checks every statement on a read-only connection
(`read_only: true`, or `--read-only` on one command). It refuses anything it cannot recognise as a read. A statement passes
only when:

- it is a single statement that starts with `SELECT`;
- none of `INSERT`, `UPDATE`, `DELETE`, `REPLACE`, `UPSERT`, `MERGE`, `SET`, `REMOVE`,
  `RETURNING`, `EXISTS`, `CREATE`, `DROP`, `ALTER`, `TRUNCATE`, `EXEC`, `EXECUTE`, `CALL`,
  `BEGIN`, `COMMIT` or `ROLLBACK` appears outside quotes;
- it has no Ion literal (`` `…` ``);
- every string, identifier and comment is terminated, and nothing outside quotes is non-ASCII.

Anything else is refused before it is sent. Quote an attribute named like a keyword (`"set"`).
Editing is refused on a read-only connection; browsing only ever scans.

For defence in depth, also give read-only connections an IAM identity that allows only reads:
`dynamodb:PartiQLSelect`, `Scan`, `GetItem`, `Query`, `DescribeTable`, `ListTables`, and for
watching `DescribeStream`, `GetShardIterator` and `GetRecords`. AWS enforces IAM; tobira enforces
the guard.

## Types

| DynamoDB | tobira |
| --- | --- |
| S | string |
| N | int when whole and within 64 bits, else decimal (exact) |
| B | bytes |
| BOOL | bool |
| NULL | NULL |
| M | object, keys sorted |
| L | list |
| SS, NS, BS | list, sorted; the native type still says SS, NS or BS |

- Sets show as lists, and writing one back stores a list, not a set.
- Times are written as RFC 3339 strings in UTC, because DynamoDB has no time type.
- NaN and infinities cannot be stored.

## Limits

| Limit | Value |
| --- | --- |
| Tables listed | 10 000 |
| Browse page | 100 items by default, at most 10 000, and at most 32 MiB |
| Sorted browsing | 10 000 items and 32 MiB |
| `in` filter | 100 values |
| Columns of `SELECT *` | the attributes of the first 1000 items |
| Describe sample | 100 items, 5 seconds, 32 MiB |
| Item size | 400 KB (a DynamoDB limit) |

## Compatibility

Tested with DynamoDB Local 3.3. Not yet tested against AWS. DynamoDB Local does not emulate:

- authentication and IAM (it accepts any key);
- throttling and provisioned capacity;
- eventual consistency of scans and global secondary indexes;
- stream shard splits and the 24-hour retention;
- the refresh of item counts and table sizes;
- some PartiQL edge cases, where its parser differs from the service.

## Troubleshooting

- **No region**: add it as the DSN host (`dynamodb://eu-west-1`) or set `AWS_REGION`.
- **"an http endpoint to a host that is not loopback needs allow_plaintext=1"**: use `https://`,
  or add `allow_plaintext=1` on a trusted network.
- **"table … has no stream"**: enable DynamoDB Streams on the table, with view type
  `NEW_AND_OLD_IMAGES`.
- **`ORDER BY` fails with a `ValidationException`**: PartiQL sorts only within one partition. Add
  `WHERE <partition key> = …`, or sort in the browse view.
- **Edits fail after you recreate a table**: run `:reload` to drop the cached key schema.
- **A read-only query with an attribute such as `set` is refused**: quote it as `"set"`.
