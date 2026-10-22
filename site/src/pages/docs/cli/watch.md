---
layout: ../../../layouts/DocsLayout.astro
title: tobira watch
---

# Stream change events

`tobira watch` streams a connection's change events to stdout, one line per event, until Ctrl-C,
`--max` or `--timeout`. It works with PostgreSQL, Greenplum, MongoDB, Redis, Amazon DynamoDB,
Cloud Firestore and Azure Cosmos DB: see [targets by driver](#targets-by-driver). Watching never
writes, so it works on read-only connections. The terminal
UI has the same feature as the [live tail](/docs/tui/#live-tail).

```sh
tobira watch local-pg orders_changed                  # a PostgreSQL LISTEN channel
tobira watch mongo shop.orders --max 10               # a MongoDB collection
tobira watch cache 'session:*' --format table         # a Redis key pattern
tobira watch dynamo orders                            # a DynamoDB table with a stream
tobira watch fs users/ada/orders                      # a Firestore collection path
```

## Targets by driver

| Driver | What it watches | Target | `op` values |
|---|---|---|---|
| PostgreSQL, Greenplum | A `LISTEN` channel. There are no change streams, so give a channel. | The channel name, such as `orders_changed` | `notify` |
| MongoDB | Change streams, on a replica set or a sharded cluster | `db.collection`, or a database name for all of its changes | MongoDB's operation type: `insert`, `update`, `replace`, `delete` and so on |
| Redis | Keyspace notifications, when the server has them enabled | A key pattern such as `session:*`, or a key prefix as `db0.user` | `insert` (new key), `update`, `delete` (also expired and evicted keys) |
| Amazon DynamoDB | DynamoDB Streams. The table needs a stream enabled. | The table name | `insert`, `update`, `delete` |
| Cloud Firestore | A snapshot listener | A collection path (`users`, `users/ada/orders`) or a document path (`users/ada`) | `insert`, `update`, `delete` |
| Azure Cosmos DB | The change feed, checked every second | A container name in the connection's database, or `database.container` | `upsert` for a created or replaced item. Deletes do not appear. |


## tobira watch

```text
tobira watch <connection|dsn> <namespace.collection|target> [--format jsonl|table] [--max N] [--timeout D] [--channel]
```

### Arguments

- `<connection|dsn>`: a connection name or a DSN, as for `tobira query`.
- `<namespace.collection|target>`: a collection as `namespace.collection`, or a driver-specific
  target: see [targets by driver](#targets-by-driver). A target without a dot, or with a glob
  character (`*`, `?`, `[`), is a target. One with a dot is a collection unless `--channel` is
  given.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--format <name>` | `jsonl` | `jsonl`: one JSON object per event, `{"time", "op", "channel", "key", "payload"}`. `table`: a header, then one aligned line per event (the time in UTC, the payload cut to 120 columns). |
| `--max <n>` | `0` | Stop after this many events and exit 0; `0` is no limit. A negative value is a usage error. |
| `--timeout <duration>` | `0` | Stop after this long and exit 0. Connecting counts, and a timeout before the watch starts exits 3. `0` is none. A negative value is a usage error. |
| `--channel` | off | Read the target as a channel, database or key pattern even when it has a dot. |

### Output

Each line is written as soon as its event arrives. The `op` values of each driver are in
[targets by driver](#targets-by-driver).

- Values are redacted like errors are: the DSN, the resolved password and `password=` values, so a
  password that also occurs in a channel name or a payload is masked there too.
- A payload field named `password` (or `pwd`, `pass`) is masked. Fields named `token`, `secret`,
  `api_key` and the like are treated as data and printed as they are.
- `jsonl` escapes control characters as JSON does, and `table` shows them as escapes (`\x1b`).

Ctrl-C exits 3 with `canceled`. A stream that breaks exits with the code of its error (2 for a lost
connection).

The AI assistant and the MCP server have no watch tool: a stream that runs until stopped does not
fit a tool call that returns one answer.
