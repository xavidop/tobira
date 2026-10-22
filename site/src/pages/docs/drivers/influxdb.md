---
layout: ../../../layouts/DocsLayout.astro
title: InfluxDB
---

# InfluxDB

The `influxdb` driver connects tobira to InfluxDB 3 (Core and Enterprise) through the official
InfluxDB 3 Go client, over Arrow Flight. It is compiled into the `tobira` binary. You query in SQL
or InfluxQL, browse measurements newest first and chart fields over time. Every connection is
read-only.

## Quick start

```sh
tobira query "influxdb3://127.0.0.1:8181/metrics" \
  "SELECT * FROM cpu ORDER BY time DESC LIMIT 10"
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: metrics
    driver: influxdb
    dsn: influxdb3://influx.example.com:8181/metrics
    password: env:METRICS_INFLUX_TOKEN
```

## Connecting

```text
influxdb3://[user:TOKEN@]host[:port][/database][?language=sql|influxql&tls=true|false&allow_plaintext=1]
```

```text
influxdb3://127.0.0.1:8181/metrics
influxdb3://influx.example.com/metrics?language=influxql
influxdb3://10.0.0.5:8181/metrics?tls=false&allow_plaintext=1
```

- The port defaults to 8181. The `influxdb://` scheme works too. IPv6 hosts go in brackets.
- **Token.** Put it in the connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`
  (see [Passwords and secrets](/docs/secrets/)). In a DSN it goes in the password position, after
  a colon: `influxdb3://:TOKEN@host/db`; the user name is ignored. A token in the user position, or
  as a `token=` parameter, is refused, because tobira could not hide it in messages. A server
  started with `--without-auth` needs no token.
- The **database** is the default namespace. Leave it out to pick one per query or in the browser.
- Connecting checks the server and the token. A rejected token is an authentication error, and
  errors never show the token.
- Any other parameter is an error, so a typo is never ignored. The connection's `options:` are not
  used.

| Parameter | Meaning |
| --- | --- |
| `language` | `sql` (the default) or `influxql`, for the query editor. Browsing, time series and the schema always use SQL |
| `tls` | `true` for HTTPS and gRPC over TLS; the default for any host that is not this machine |
| `allow_plaintext` | with `tls=false`, allow plain HTTP to a host that is not this machine |

**TLS.** TLS (1.2 or later) is the default for any host other than `localhost` or a loopback
address, where plain HTTP is the default. Plain HTTP to another host needs both `tls=false` and
`allow_plaintext=1`, and a token sent that way writes a warning to the `--debug` log. InfluxDB
Cloud (`*.influxdata.com`) and Cloud Dedicated (`*.influxdb.io`) refuse `tls=false` even with it.
tobira never follows an HTTP redirect, so the token cannot be forwarded to another host.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, tables, describe | yes; databases are the namespaces, without `_internal` |
| Browse | yes; newest first, with offset paging |
| Time series | yes; raw points, or windows aggregated with mean, sum, min, max, count or last |
| Read-only | always: every connection is read-only |
| Editing, transactions, explain, completion, native export, watching | no |

There is no editing: InfluxDB writes use line protocol, which tobira does not send. You can still
run `EXPLAIN SELECT …` as a query.

## Queries

A query is one SQL statement, or one InfluxQL statement with `language=influxql`.

```sql
SELECT * FROM cpu WHERE host = 'web-1' AND time > now() - INTERVAL '1 hour' ORDER BY time DESC
SELECT date_bin(INTERVAL '5 minutes', time) AS t, avg(usage) FROM cpu GROUP BY t ORDER BY t
SHOW TABLES
EXPLAIN SELECT * FROM cpu LIMIT 10
```

```text
SELECT mean("usage") FROM "cpu" WHERE time > now() - 1h GROUP BY time(5m)
```

- Parameters are written `$1`, `$2` …. Integers, floats, strings, booleans, decimals (sent as
  strings), times (sent as RFC 3339 strings) and NULL are supported.
- Results stream batch by batch. Closing a result or cancelling stops the server's stream at once.
- Without a database in the DSN, each query needs one picked.

## Browsing

- **Databases** are the namespaces, without `_internal`. The DSN's database is the default.
- **Tables** (measurements) are listed, at most 10,000.
- **Describe** shows, for each column, whether it is a tag, a field or the timestamp, and its Arrow
  type. The key is the series key: the tags, then `time`.
- **Order and paging.** Rows come newest first, then by tag, so pages never repeat or skip a row.
  You can sort by any column; NULLs sort last ascending. A page is 100 rows by default, at most
  10,000.
- **Filters**: equals, not equals, comparisons, contains and starts with (on the value as text),
  in a list, is null and is not null.
- A **raw filter** is a SQL condition, combined with the others. It must hold no comment and no
  `;`, and keep its parentheses balanced.

## Time series

In the TUI, `:chart <field> [window]` charts a field of the open table and `:range` sets the time
range (see [time series in the TUI](/docs/tui/kind-views/#time-series)).

- Pick a field, a time range (start inclusive, end exclusive) and optionally a window, an
  aggregate and tags to group by.
- Without a window you get the raw points of the field, in time order.
- With a window, points are grouped with `date_bin`. Windows are aligned to midnight UTC on
  1 January 1970, each row's time is its window's start, and windows with no points for that field
  are left out. The aggregate is `mean` (the default), `sum`, `min`, `max`, `count` or `last`.
- Rows where the field is NULL are skipped.
- One request returns at most 100,000 rows.
- Times must lie between the years 1678 and 2262, the range of nanoseconds since 1970.

## Read-only guard

Every statement is read-only, on every connection, whatever `read_only` says. There are two layers:

1. **The server.** InfluxDB 3's query API cannot write: `INSERT`, `DELETE`, `COPY`,
   `CREATE TABLE … AS`, `SELECT … INTO` and `SET` all fail on the server.
2. **An allow-list, before anything is sent.** The text must be exactly one `SELECT`,
   `WITH … SELECT` (SQL only), `SHOW`, or `EXPLAIN [ANALYZE] [VERBOSE]` of a query. It fails
   closed, so these are refused too:
   - the word `INTO` anywhere outside quotes;
   - in SQL, the words `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `COPY`, `CREATE`, `DROP`, `ALTER`
     and `TRUNCATE` anywhere outside quotes. Write a column with such a name in double quotes
     (`"update"`);
   - a second statement, `DESCRIBE`, and every other statement kind;
   - text tobira cannot read the way the server does: an unterminated string, identifier, comment
     or regex, a nested block comment, a `$$` string, or a backtick identifier (use double
     quotes);
   - non-ASCII text outside quotes, comments included. Double-quote names with accented letters.
   - in InfluxQL, a `;` inside a regex.

## Types

| InfluxDB 3 (Arrow) | tobira |
| --- | --- |
| signed integers; UInt8 to UInt32 | int |
| UInt64 | decimal (it can exceed the int range) |
| Float64 | float |
| Decimal128/256 (from a `CAST`) | decimal |
| Boolean | bool |
| Utf8, tags | string |
| Binary | bytes |
| Timestamp, Date | time (UTC) |
| Duration, Time | string |
| List, Struct | list, object |
| anything else | unknown, with Arrow's display text |

## Limits

| Limit | Value |
| --- | --- |
| Tables listed | 10,000 |
| Browse page | 100 rows by default, at most 10,000 |
| Time-series request | 100,000 rows |
| Times | the years 1678 to 2262 |

## Compatibility

Tested with InfluxDB 3.6 Core, with authentication on. The driver speaks the InfluxDB 3 API only.

## Troubleshooting

- **The TLS handshake fails, and the error says to add `tls=false&allow_plaintext=1`**: the server
  speaks plain HTTP. Add both on a trusted network, or put the server behind TLS.
- **"plain http to … (not loopback) needs allow_plaintext=1 as well as tls=false"**: add
  `allow_plaintext=1`.
- **"tls=false is refused for InfluxDB Cloud"**: Cloud needs TLS. Remove `tls=false`.
- **The first query stalls for seconds on `localhost`**: the client tries IPv6 first and the server
  only listens on IPv4 (as with Docker Desktop's port forwarding). Use `127.0.0.1`.
- **A query with a column named `update` or `delete` is refused**: double-quote the name.
- **No database**: add it to the DSN path, or pick one in the browser.
