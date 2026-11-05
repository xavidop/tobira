---
layout: ../../../layouts/DocsLayout.astro
title: tobira federate
---

# Run a cross-source query

`tobira federate` runs one query over several connections. Each `@source` directive stages the rows
of its own native query, read-only, into a scratch SQLite database, and a final SQLite statement
joins them there. The result prints like `tobira query` does, or goes to a file. The shape of the
statement, its limits and the type mapping are on [Cross-source queries](/docs/cross-source/).

```sh
tobira federate -f weekly.sql --format csv
tobira federate -f weekly.sql --explain
tobira federate -f weekly.sql --output ~/weekly.csv
tobira federate "-- @source o = shop: select user_id, total from orders where paid = 1
-- @source u = crm: select id, name from users
select u.name, count(*) as orders, sum(o.total) as spent
from o join u on u.id = o.user_id group by u.name order by spent desc"
```

## tobira federate

```text
tobira federate [query] [flags]
```

### Arguments

- `[query]`: the cross-source statement: one or more `-- @source <alias> = <connection>[/<namespace>]: <query>`
  lines, then one SQLite statement over the alias tables. Omit it when you use `-f`. There is no
  connection argument: the statement names its connections itself, and each must be a connection
  of the [config file](/docs/configuration/); a DSN is not accepted.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-f, --file <path>` | | Read the statement from a file; `-f -` reads stdin. Not allowed together with a query argument. |
| `--format <name>` | `table` on a terminal, else `json` | `table`, `json`, `ndjson` or `csv`, as for [`tobira query`](/docs/cli/query/#output-formats). |
| `--limit <n>` | `1000` | Maximum rows of the final result; `0` is no limit. When more rows exist the output says it was truncated. The sources have their own cap, `--max-source-rows`. |
| `--max-source-rows <n>` | `federate.max_source_rows`, else `10000` | Most rows staged from each source, 1 to 1,000,000. A source with more rows is cut, and the result's stats carry a warning naming it. |
| `--timeout <duration>` | `30s` | Timeout for the whole run, connecting included; `0` is none. |
| `--explain` | off | Print each source with its connection and query, then the final statement, instead of running them. It contacts no database. |
| `--output <path>` | | Write the result to a file instead of stdout. The format comes from `--format`, else the extension; every row is kept and there is no timeout unless `--limit` or `--timeout` is set. See [Export to a file](/docs/cli/query/#export-to-a-file). Not allowed with `--explain` or a `@sink`. |
| `--apply` | off | With a `-- @sink` line: insert the result's rows into the sink collection. See [Writing the result into a collection](#writing-the-result-into-a-collection). |
| `--dry-run` | off | With a `-- @sink` line: print the rows `--apply` would insert, and write nothing. |
| `--config <path>` | | Config file path (see [Configuration](/docs/configuration/)). |
| `--debug` | off | Write JSON debug logs to the debug log file. |

A negative `--limit` or `--timeout`, or a `--max-source-rows` out of range, is a usage error. Ctrl-C
cancels the run and exits 3 with `canceled`.

### Read-only, always

Every source runs read-only, whatever its connection allows, and the connections are opened
read-only at the protocol level, as `tobira query --read-only` opens them. A source query that
writes is refused before anything runs, and so is a final statement that is not a read; both exit 4.
A source that fails for any other reason fails the whole run, with the alias and the connection
in the message.

### Writing the result into a collection

```sh
tobira federate -f weekly.sql --dry-run     # the rows that would be inserted, and where
tobira federate -f weekly.sql --apply       # insert them
```

A statement whose last directive is `-- @sink <connection>[/<namespace>]: <collection>` writes
its result into that collection: one insert per row, keyed by the result's column names, with
`--apply`. `--dry-run` prints the rows instead and writes nothing; without either flag the
statement is refused (exit 1), so a saved file with a sink never writes by accident. `--explain`
names the sink in the plan.

- The sink connection must be configured, must not be read-only (exit 4), its driver must edit
  rows and the collection must exist; all of that is checked before any source is staged. Only
  the sink connection is opened writable, and only with `--apply`; the sources still run read-only
  on it.
- Where the driver has transactions the rows go in one transaction, so a failure (a key already
  taken, say) writes nothing and the message says `inserted 0 of N rows`. Without transactions
  they go one at a time and stop at the first error, which the message counts.
- The write is recorded in the [query history](/docs/history/) on the sink's connection.
- The target driver coerces the values, as it does for inline edits; a decimal, time or JSON
  value arrives as the SQLite result holds it (see the [type mapping](/docs/cross-source/#types)).

### Stats and history

The `json` output's `stats`, and the `table` footer, carry one warning per source with the rows it
staged, such as `source orders (local-pg): 10000 rows, more existed` when it was cut. Each staged
query is recorded in the [query history](/docs/history/) on its own connection, and the whole
statement once more under the connection name `federated`, so `tobira history --conn local-pg`
shows what ran there and `tobira history --conn federated` lists the cross-source runs.

## Exit codes

`tobira federate` uses the [shared exit codes](/docs/cli/#exit-codes): 1 for a bad directive or a
connection that is not configured, 2 when a source connection cannot be opened, 3 for a query error
in a source or the final statement, 4 for a write refused as read-only.
