---
layout: ../../../layouts/DocsLayout.astro
title: MongoDB
---

# MongoDB

The `mongodb` driver connects tobira to MongoDB through the official Go driver. It is compiled
into the `tobira` binary. You query with MongoDB command documents in JSON, browse and edit
collections, explain queries and follow change streams.

## Quick start

```sh
tobira query "mongodb://reader@localhost:27017/shop?authSource=admin" \
  '{"find":"orders","filter":{"status":"paid"},"limit":10}' --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: shop
    driver: mongodb
    dsn: mongodb://reader@localhost:27017/shop?authSource=admin
    password: env:SHOP_MONGO_PASSWORD
    read_only: true
```

## Connecting

```text
mongodb://user@localhost:27017/shop
mongodb://user@db1,db2,db3/shop?replicaSet=rs0&authSource=admin
mongodb+srv://user@cluster0.example.net/shop
mongodb://user@db.example.com/shop?tls=true&tlsCAFile=/etc/ssl/mongo-ca.pem
```

- Every URI option of the official MongoDB driver works: `authSource`, `authMechanism`,
  `replicaSet`, `directConnection`, `readPreference`, `tls`, `tlsCAFile` and the rest.
- The database in the path is the default. Without one it is `test`, as in mongosh.
- **Password.** Keep it out of the DSN. Use the connection's `password:` reference or
  `TOBIRA_CONN_<NAME>_PASSWORD` (see [Passwords and secrets](/docs/secrets/)). It replaces the
  password in the URI, so the URI must name the user.
- **TLS** follows the URI: `tls=true` turns it on, and `mongodb+srv://` turns it on by default.
  MongoDB Atlas needs TLS.
- The connection's `options:` are refused ("unsupported option"). Put every setting in the URI.
- Unless the URI sets `serverSelectionTimeoutMS`, tobira waits at most 10 seconds for a server.
- Passwords never appear in errors, and neither do other secrets in the URI, such as an
  `AWS_SESSION_TOKEN` in `authMechanismProperties` or a key file password.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, collections, describe | yes; a query is one command document; describe samples 100 documents |
| Browse | yes; paged by `_id`, or by skip when sorted |
| Editing | yes; insert, update and delete of one document, by `_id` |
| Explain | yes; the `explain` command; analyze uses `executionStats`, which never applies a write |
| Completion | yes; commands, options, operators, collections and field paths |
| Watching | yes; change streams, on a replica set or sharded cluster |
| Transactions, native export | no (`:export` and `--output` use tobira's own writers) |

## Queries

A query is one database command document in JSON or extended JSON (`{"$oid": …}`,
`{"$date": …}`, `{"$numberDecimal": …}`). It runs in the current database. The first key names
the command, and key order is kept.

```json
{"find":"orders","filter":{"status":"paid"},"sort":{"_id":-1},"limit":10}
{"aggregate":"orders","pipeline":[{"$group":{"_id":"$status","n":{"$sum":1}}}]}
{"count":"orders","query":{"status":"paid"}}
{"distinct":"orders","key":"status"}
{"listIndexes":"orders"}
```

- `find`, `aggregate`, `listCollections` and `listIndexes` stream their documents as rows.
  `distinct` returns one row per value. Any other command returns its reply as one row.
- Documents differ in fields, so the columns are the union of the fields of the first 1000
  documents (or the first 32 MiB), `_id` first. Every row is still shown. Fields that only later
  documents hold are left out, with a warning; add a projection to see them.
- Rows from `find` can be edited in place, since they name their collection and `_id`.
- Write commands (`insert`, `update`, `delete`, `findAndModify`) report the number of documents
  they changed.
- Choose the database with the namespace, not `$db`. Query parameters are not supported: put the
  values in the document. Session and transaction fields (`lsid`, `txnNumber`,
  `startTransaction`, `autocommit`) are refused; tobira manages sessions.
- Text after the document, or anything but one JSON object, is a syntax error.
- `find` on a collection that does not exist returns nothing, as in MongoDB.

## Browsing

- **Databases** are the ones you may read, without `admin`, `local` and `config`. The DSN's
  database is always listed, even before it holds data.
- **Collections** are listed by name, `system.*` left out. Views appear as views.
- **Describe** reads a random sample of up to 100 documents, for at most 5 seconds and 32 MiB,
  and infers the fields: nested fields for embedded documents and for documents inside arrays,
  mixed types shown as unions such as `int|string`, and fields some documents lack marked
  nullable. It also lists the indexes, the `_id` key and the estimated document count.
- **Filters**: equals, not equals, less or greater than, contains and starts with (compared as
  text), in a list, is null (null or missing) and is not null. A 24-digit hex string also matches
  the ObjectId it spells. A field may be a dotted path such as `address.city`. A raw filter is a
  JSON filter document, combined with the other conditions.
- **Paging**: unsorted pages follow `_id`, which stays fast on deep pages and handles mixed `_id`
  types. Sorted pages use skip. NULLs sort first ascending, as in MongoDB. The default page is
  100 documents. A page of very large documents ends early at 32 MiB, and the next page continues
  after it.

## Editing

- Each edit addresses one document by `_id`. An edit that matches no document, or more than one,
  is refused.
- An update sets each changed field with `$set`. Setting a field to NULL stores `null`. Removing a
  field is not supported in inline editing (a missing field also shows as NULL).
- Field names that contain a `.` cannot be edited, since MongoDB would read them as nested paths.
  Edit the whole parent document instead.
- `_id` cannot change. An insert without `_id` gets a new ObjectId.
- A duplicate `_id` or a unique index violation is reported as a constraint error.

## Explain

`tobira query --explain` runs MongoDB's `explain` with the `queryPlanner` verbosity and shows the
winning plan as a tree, with the full reply as JSON. With analyze it uses `executionStats`.
MongoDB never applies a write it explains.

## Completion

The editor completes collection names inside `find`, `aggregate`, `count`, `distinct`,
`listIndexes` and `collStats`; query operators, aggregation stages and `$field` references after a
`$`; and otherwise command names, command options and the field paths of the command's
collection. The catalog is cached for 60 seconds; `:reload` refreshes it.

## Watching

tobira can follow a collection or a whole database through a change stream. It reports inserts,
updates, replaces and deletes as they happen, with the full document for updates.

- In the TUI, `W` (or `:watch`) opens a [live tail](/docs/tui/#live-tail) of the open collection,
  and `:watch <database>` one of a whole database.
- `tobira watch <connection> <db.collection>` streams the same events as JSON lines.

Change streams need a replica set or a sharded cluster. On a standalone server, watching is
reported as not supported.

## Read-only guard

`read_only: true` on a connection, or `--read-only` on one command, is a hard guard. MongoDB has
no read-only session, so tobira checks the command document itself, exactly as it is sent,
against an allow-list:

- Only `find`, `aggregate`, `count`, `distinct`, `listCollections`, `listIndexes`, `collStats`,
  `dbStats`, and `explain` of `find`, `aggregate`, `count` or `distinct`, run.
- Everything else is refused: every write command, `findAndModify`, `bulkWrite`, `mapReduce`,
  `eval`, DDL, index, user, role and server commands, `getMore`, `killCursors` and transaction
  commands.
- An aggregation with `$out` or `$merge` anywhere in it, sub-pipelines included, is refused. A
  raw browse filter is checked the same way.
- Session, transaction and write-concern fields, top-level `$` fields and repeated top-level
  fields are refused.
- A read concern must be `local`, `available`, `majority` or `snapshot`. `linearizable` is
  refused, because each such read writes a no-op entry to the primary's oplog. Reads use `local`
  unless the document names one.
- Edits on a read-only connection are refused before anything is sent.

Server-side JavaScript (`$where`, `$function`, `$accumulator`) is allowed, since it cannot write.
It can still use server CPU: read-only is not a resource limit.

## Types

| MongoDB | tobira |
| --- | --- |
| double | float |
| int, long | int |
| decimal | decimal (exact) |
| string, symbol | string |
| bool | bool |
| date | time (UTC) |
| null, undefined | NULL |
| embedded document | object (key order kept) |
| array | list |
| binary (subtype 0) | bytes |
| ObjectId | shown as its hex string |
| UUID (binary subtype 4) | shown in its canonical form |
| timestamp, regex, JavaScript, other binary, minKey, maxKey | shown as extended JSON |

ObjectIds, UUIDs and the other MongoDB-specific values are written back unchanged when you edit a
document. Values you type are stored as `int` when they fit in 32 bits and `long` otherwise.

## Limits

| Limit | Value |
| --- | --- |
| Collections listed per database | 10000 (the last entry says when the list stopped) |
| Browse page | 100 documents by default, at most 10000, and at most 32 MiB |
| Columns of a query result | the fields of the first 1000 documents or 32 MiB |
| Describe sample | 100 documents, 5 seconds, 32 MiB |
| Completions | 200 |

## Compatibility

Tested with MongoDB 7.0 and 8, as a single-node replica set with authentication and as a standalone
server.

## Troubleshooting

- **A connection error after about 10 seconds**: no server answered in time. Check the host and
  port, and for a replica set check that the member names it advertises resolve from your
  machine. `directConnection=true` connects to one member only.
- **Authentication failed**: the user often lives in another database. Add `authSource=admin`.
- **Watching is not supported**: the server is standalone. Change streams need a replica set; a
  single-node replica set (`--replSet rs0`, then `rs.initiate()`) is enough.
- **A field is missing from the result**: it only appears in later documents. Add a projection
  that names it.
- **Refused on a read-only connection**: the command is not on the allow-list above. Run it on a
  writable connection.
