---
layout: ../../../layouts/DocsLayout.astro
title: Redis
---

# Redis

The `redis` driver connects tobira to Redis through `go-redis`. It is compiled into the `tobira`
binary. You run Redis commands, browse keys grouped by prefix, edit one key at a time and watch
keyspace notifications.

## Quick start

```sh
tobira query "redis://localhost:6379/0" "HGETALL user:1" --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: cache
    driver: redis
    dsn: rediss://default@cache.example.com:6380/0
    password: env:CACHE_REDIS_PASSWORD
    read_only: true
```

## Connecting

```text
redis://localhost:6379/0
redis://user@cache.example.com:6379/2?dial_timeout=3s
rediss://cache.example.com:6380/0
unix:///var/run/redis.sock?db=1
```

- The path is the database number.
- **TLS**: `rediss://` uses TLS and verifies the server certificate. `skip_verify=true` turns
  verification off; use it only for testing.
- A `unix://` socket has no Redis scheme, so select it with `driver: redis` on the connection.
- Parameters are go-redis's (`dial_timeout`, `read_timeout`, `pool_size`, `client_name`,
  `skip_verify` and so on). An unknown one is refused.
- **Password.** Keep it out of the DSN. A password from the connection's `password:` reference or
  `TOBIRA_CONN_<NAME>_PASSWORD` replaces the DSN's (see [Passwords and secrets](/docs/secrets/)).
  Passwords never appear in errors.
- The connection's `options:` are refused. Put every setting in the DSN.
- Cluster mode is not supported. On a read-only connection to a cluster replica, tobira sends
  `READONLY` so the replica serves reads.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, collections, describe | yes; logical databases are the namespaces, key prefixes are the collections |
| Browse | yes; keys with their type, TTL and a value preview, paged with `SCAN` |
| Editing | yes; insert, update and delete of one key at a time, by key and type |
| Watching | yes; keyspace notifications, when the server has them on |
| Explain, transactions, completion, native export | no |

## Databases and collections

- **Databases** `0` to `N-1` are the namespaces, with `N` from the server's `databases` setting
  (16 when the server will not say). Each shows its key count.
- **Collections** are `*`, which holds every key, plus the key prefixes found by sampling up to
  10 000 keys with `SCAN` and cutting each key at its first `:`. The collection `user` holds every
  `user:…` key. A key with no `:` (say `session_9f8e7d6c`, or a bare `user` next to `user:1`) is
  listed only under `*`, never as a collection or inside one, because its name is the whole key.
  At most 500 prefixes are listed; when there are more, the last one's comment says how many were
  left out.
- A prefix whose keys are all **hashes** is a collection of records: its hash fields also show as
  columns.

On Redis, names are data. Key prefixes (as collection names) and hash field names (as columns) are
schema to the [AI assistant](/docs/ai/) and the [MCP server](/docs/mcp/), so they are sent to the
model even under `send_rows: never`. Values and whole key names are not, except a key that is
itself a prefix of other keys (`user` next to `user:1`). If your key prefixes carry secrets, do not
give the model that connection.

## Queries

A query is one Redis command, quoted as in `redis-cli`: double quotes take `\n`, `\"` and `\xHH`
escapes, single quotes are literal.

```text
HGETALL user:1
ZRANGE leaderboard 0 -1 WITHSCORES
SET greeting "hello world"
SCAN 0 MATCH user:* COUNT 100
FT.SEARCH idx "@age:[30 40]" LIMIT 0 50
```

Replies show as tables:

| Command | Columns |
| --- | --- |
| `HGETALL`, `HSCAN` | field, value |
| `MGET` | key, value |
| `ZRANGE … WITHSCORES` | member, score |
| `SCAN` | key (the next cursor appears as a notice) |
| `XRANGE` | id, fields |
| `INFO` | section, key, value |
| `FT.SEARCH` | `_key` plus the document fields, typed (RediSearch indexes database 0 only) |
| anything else | one `value` column, typed by what it holds |

- The query's database comes from the namespace. `SELECT` is refused.
- Query parameters are not supported: put the values in the command line.
- Reads of a whole value that would return more than 100 000 elements or 16 MiB (`HGETALL`,
  `SMEMBERS`, `LRANGE key 0 -1`, `XRANGE key - +`, `GET` and similar) are refused with "too large;
  use HSCAN/SSCAN/ZSCAN or a ranged command".
- Passwords in `CONFIG GET` and `ACL LIST` / `ACL GETUSER` replies show as `xxxxx`.
- Commands that would change or hold the shared connection are refused on every connection:
  `SELECT`, `MULTI`, `EXEC` and `WATCH`, subscribing, `MONITOR`, `HELLO`, `AUTH`, `RESET`, blocking
  pops and blocking stream reads, among others.

## Browsing

Every row shows the key, its type, its TTL in seconds (`-1` means no expiry) and a preview of its
value:

| Type | Preview |
| --- | --- |
| string | the first 4 KiB, ending with `…` when cut |
| hash | up to 100 fields |
| list | the first 100 elements |
| set | up to 100 members, sorted |
| sorted set | the first 100 members with their scores |
| stream | the first 100 entries |
| JSON (RedisJSON) | the document, cut at 64 KiB |

When a preview is shorter than the real value, the row's `_truncated` column is true. The grid
hides that column; the row detail shows it. Such a value cannot be edited inline: saving it is
refused with "value is too large to edit inline; use a query command". Change it with a command
such as `HSET`, `RPUSH` or `SET` instead. Its TTL can still be changed, and it can be deleted.

Exporting a collection (`:export` in the TUI) writes these previews, not the full values. tobira
says how many values were previews when the export ends ("2 values were previews, not full
values"); see [previews](/docs/export/#previews).

In a collection of records, each hash field gets the narrowest type all sampled values share:
`7` is a number but `007` stays text, `12.50` is a decimal, `true` and `false` are booleans, an
RFC 3339 timestamp is a time, and a JSON object or array is JSON. When every key ends with the value
of one field (an `id` that also appears in the key, as in `user:7`), that field is the key column.

- **Filters**: every operator, evaluated by tobira on the shown columns. A raw filter is a Redis
  glob pattern the key must match, such as `user:1*`.
- **Order and paging**: unsorted rows come in key-name order, and each page continues after the
  last key of the one before, so no key repeats or goes missing between pages. The cost is that
  every page scans the whole database with `SCAN`, whatever the page size. That scan is capped at
  about 200 000 keys or 5 seconds per page. On a database too large to scan within that, browsing
  reports an error instead of showing an incomplete page; read keys with a `SCAN` query instead.
- **Sorting** reads the whole collection first, so it is refused for collections of more than
  10 000 keys. NULLs sort last.

tobira never sends `KEYS`.

## Editing

Edits change one key at a time, atomically: if the key changes while you edit, the edit fails
instead of half-applying.

- **Insert** creates a key with a value and an optional type (`string`, `hash`, `list`, `set`,
  `zset` or `ReJSON-RL` for RedisJSON) and TTL. In a collection of records, fill in the field
  columns instead; the key comes from the key column. A key that already exists is refused.
- **Update** replaces the value (the TTL is kept), changes the TTL (`-1` removes it), or edits hash
  fields (clearing a field deletes it). Renaming a key is not supported.
- **Delete** removes the key.

Streams cannot be edited. Long values whose preview was cut must be edited with a query.

## Watching

Watching a collection or key pattern streams Redis keyspace notifications: a key created, updated,
deleted, expired or evicted.

- The server must have `notify-keyspace-events` on (for example `KEA`). Otherwise watching reports
  "not supported". tobira never changes the server's configuration.
- On managed services that hide the setting, tobira subscribes anyway.
- In the TUI, `W` (or `:watch`) opens a [live tail](/docs/tui/#live-tail) of the open key pattern,
  and `:watch <glob>` one of any key pattern.
- `tobira watch <connection> '<glob>'` streams the same events as JSON lines.

## Read-only guard

`read_only: true` on a connection, or `--read-only` on one command, is a hard guard with two
layers:

1. **An allow-list, before anything is sent.** Only reads are accepted: `GET`, `MGET`, `STRLEN`,
   `GETRANGE`, the hash, list, set, sorted-set and stream reads (`HGETALL`, `LRANGE`, `SMEMBERS`,
   `ZRANGE`, `XRANGE`, the `SCAN` family and so on), `XINFO`, `TYPE`, `TTL`, `PTTL`, `EXISTS`,
   `DBSIZE`, `INFO`, `JSON.GET`, `JSON.TYPE`, `MEMORY USAGE`, `OBJECT ENCODING`, `FT.SEARCH`,
   `FT.INFO` and `FT._LIST`. A scan may ask for at most 10 000 keys per call. Everything else is
   refused, including scripts (`EVAL`, `FCALL`, even their read-only forms), `FUNCTION`, `MULTI`,
   `CONFIG` (`GET` too), `SELECT`, `MODULE`, `DEBUG`, `FLUSHALL`, `KEYS`, `SORT` and every write.
   Command names must be plain ASCII, so look-alike letters cannot sneak a command through.
2. **The server's own flags.** tobira asks the server how it classifies each command and refuses
   any it marks as a write, admin, pub/sub or blocking command. This catches a server where
   `rename-command` gave a write command the name of a read. If the server will not answer (an ACL
   without the `COMMAND` permission), read-only queries are refused.

Edits on a read-only connection are refused before anything is sent. Browsing and describe only
send reads.

## Limits

| Limit | Value |
| --- | --- |
| Keys sampled to find prefixes | 10 000 |
| Prefixes listed | 500 |
| Value previews | 4 KiB of a string, 100 elements, 64 KiB of JSON |
| Whole-value reads in a query | 100 000 elements or 16 MiB |
| Scan per browse page | about 200 000 keys or 5 seconds |
| Sorted browsing | collections of up to 10 000 keys |
| `COUNT` of a scan on a read-only connection | 1 to 10 000 |

Cluster mode is not supported: a `MOVED` or `ASK` reply is reported as not supported.

## Compatibility

Tested with Redis 8, including its RedisJSON and RediSearch modules.

## Troubleshooting

- **"browsing needs a full SCAN of the database per page, and it did not finish …"**: use a `SCAN 0 MATCH prefix:* COUNT 1000`
  query, or narrow the collection with a raw filter pattern.
- **"value is too large to edit inline; use a query command"**: the row shows a preview. Change the
  value with `SET`, `HSET`, `RPUSH` and similar commands.
- **"too large; use HSCAN/SSCAN/ZSCAN or a ranged command"**: read the value in parts, for example
  `HSCAN user:1 0 COUNT 1000` or `LRANGE queue 0 999`.
- **Watching is not supported**: turn on keyspace notifications, for example
  `CONFIG SET notify-keyspace-events KEA`, or set it in `redis.conf`.
- **Read-only queries are all refused**: the ACL user cannot run `COMMAND`. Grant `+command`, or
  use a writable connection.
- **"unsupported option"**: the connection has `options:`. Move the settings into DSN parameters.
