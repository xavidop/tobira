---
layout: ../../../layouts/DocsLayout.astro
title: BigQuery
---

# BigQuery

The `bigquery` driver connects to Google BigQuery. It is built into the `tobira` binary and uses
the official Go client, `cloud.google.com/go/bigquery`, over the BigQuery REST API. You write
GoogleSQL. You can query, browse, explain (as a dry run) and use completion. Editing rows is not
available (see [Editing](#editing)).

## Quick start

```sh
tobira query "bigquery://my-project/sales?location=EU" "select count(*) from orders" --read-only
```

```yaml
connections:
  - name: warehouse
    dsn: bigquery://my-project/sales?location=EU
    password: file:///home/me/.config/gcloud/tobira-reader.json
    read_only: true
```

## Connecting

```text
bigquery://my-project
bigquery://my-project/sales
bigquery://my-project/sales?location=EU&max_bytes_billed=10737418240
bigquery://test-project/sales?emulator=localhost:9050
```

- The host is the project id. The optional path is the default dataset, used for unqualified
  table names and opened first in the terminal UI.

| Parameter | Effect |
| --- | --- |
| `location` | Where jobs run (`US`, `EU`, `europe-west1` and so on). |
| `max_bytes_billed` | Caps every query job, in bytes. 1 GiB by default; `0` means no cap from tobira. A query that would bill more fails instead of running. |
| `emulator=host:port` | Talk plain HTTP to a BigQuery emulator, without credentials. |

Any other parameter is refused. The connection talks to Google over HTTPS; the
`allow_plaintext=1` rule does not apply, since only the emulator uses plain HTTP and it gets no
credentials.

### Credentials

- The connection's `password:` must be a service-account key: the JSON file Google Cloud gives
  you (`"type": "service_account"`). Use a [secret reference](/docs/secrets/) such as `file://`
  or `keyring:`. Other credential files are refused, because some of them (`external_account`)
  can make the auth library run a program.
- With no password, the driver uses Application Default Credentials
  (`gcloud auth application-default login`, or the attached service account on Google Cloud).
- A DSN holding a credential (`user:pass@`, `?credentials=...`, `?key=...`) is refused.
- The key never appears in errors or logs. tobira never follows an HTTP redirect, so your tokens
  cannot be forwarded to another server.

For read-only use, grant the key `roles/bigquery.dataViewer` and `roles/bigquery.jobUser`.

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes (datasets are the namespaces) |
| Explain | yes (a dry run: statement type, estimated bytes, schema; no `ANALYZE`) |
| Completion | yes (GoogleSQL keywords, datasets, tables, columns) |
| Edit rows, transactions, native export | no |
| Read-only guard | yes |

## Queries

Write GoogleSQL, one statement per query.

```sh
tobira query warehouse "select status, count(*) from orders group by status"
tobira query warehouse --explain "select * from orders where order_date > '2026-01-01'"
```

- Each query is a job named `tobira_<random>`; there is no session between queries.
- `?` placeholders bind parameters. A NULL parameter is a NULL `STRING`.
- On a writable connection anything runs, scripts included, and DML reports the rows it changed.
- Cancelling a query returns at once and cancels the job on the
  server, so it stops billing.

## Browse

A table opened with no filter and no sort pages through `tabledata.list`, which BigQuery does not
bill. Filters, sorts and views run a `SELECT` with `LIMIT`/`OFFSET`, ordered by your sort, then
the declared primary key, then every orderable column; that query is billed (under the byte cap)
and always passes the read-only guard. Each page re-runs the query and is billed again for a full
scan of the columns read (`LIMIT` does not reduce what BigQuery scans), so page large tables
without a filter or sort. `ARRAY` and `STRUCT` columns can be filtered only through
a raw `WHERE` filter.

## Describe

The schema with nested `STRUCT` fields, `REQUIRED` columns, column descriptions and default
expressions, the declared (unenforced) primary and foreign keys, partitioning and clustering
shown as indexes, row and byte counts, and a view's query.

## Explain

A dry run: the statement type, the estimated bytes processed, the byte cap, the result schema and
the referenced tables. Nothing is executed, so explaining a write is safe.

## Editing

The grid is read-only for BigQuery. Run DML as a query on a writable connection.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, or when you
run `:readonly on`. tobira then protects you in three layers:

1. **An allow-list, before anything is sent.** The text must be one query starting with
   `SELECT` or `WITH`. Several statements (so every script), text that cannot be read to its end,
   a `#legacySQL` directive, and the words `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `TRUNCATE`,
   `CREATE`, `DROP`, `ALTER`, `EXPORT`, `LOAD`, `DECLARE`, `SET`, `EXECUTE`, `CALL`, `BEGIN`,
   `COMMIT`, `ROLLBACK`, `GRANT`, `REVOKE`, `ASSERT`, `RAISE` and `EXTERNAL_QUERY` outside strings
   are refused. Write a column with one of those names in backticks: `` `update` ``. Quoting
   does not get `EXTERNAL_QUERY` through (`` `EXTERNAL_QUERY` ``, `` `region-us.external_query` ``
   are refused too), nor does a backslash escape in a backtick name.
2. **A dry run.** BigQuery validates the query without running it, and the statement type it
   reports must be `SELECT`. Scripts, DML, DDL and `EXPORT DATA` are refused before anything
   runs.
3. **A byte cap.** Every job carries `maximumBytesBilled` (see `max_bytes_billed`).

Refused statements make `tobira query` exit with code 4.

**What it cannot stop.** A `SELECT` that calls a remote function or an ML function still
reaches outside BigQuery. Restrict those with IAM.

## Types

| BigQuery | tobira |
| --- | --- |
| INT64 | integer |
| FLOAT64 | float |
| NUMERIC, BIGNUMERIC | exact decimal |
| BOOL | boolean |
| STRING | text |
| BYTES | bytes |
| JSON | JSON |
| TIMESTAMP, DATE, DATETIME | time (UTC) |
| TIME, GEOGRAPHY, INTERVAL, RANGE | text |
| ARRAY | list |
| STRUCT | object |

A value holding more than 10000 array elements is shown as a placeholder; select fewer elements.
A single text, bytes or JSON value longer than 1 MiB is cut there, with a warning on the result.

## Limits

- No row edits and no transactions.
- Browsing with a filter or sort re-runs a billed query for every page.
- Values with more than 10000 array elements show as a placeholder; text, bytes and JSON values
  are cut at 1 MiB.

## Compatibility

Tested with the [goccy BigQuery emulator](https://github.com/goccy/bigquery-emulator) 0.8.1. Not
yet tested against Google Cloud. The emulator reports every statement as a `SELECT` and ignores the
byte cap, so the read-only dry-run check and `maximum_bytes_billed` have been tested only against a
local stand-in for the BigQuery API.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `the password must be a service-account key` | Point `password:` at a service-account JSON key, or remove it to use Application Default Credentials. |
| `credentials must not be in the DSN` | Move the key to the connection's `password:`. |
| `malformed DSN: unknown parameter` | Only `location`, `max_bytes_billed` and `emulator` are accepted. |
| `the query would bill more than max_bytes_billed allows` | Narrow the query (fewer columns, a partition filter), or raise `max_bytes_billed`. |
| `read-only violation` | The statement is not a single `SELECT`, or uses a refused word. Put a column named like a refused word in backticks. |
