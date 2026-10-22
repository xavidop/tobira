---
layout: ../../../../layouts/DocsLayout.astro
title: "PostgreSQL: LISTEN and COPY export"
---

# PostgreSQL: LISTEN and COPY export

The [PostgreSQL driver](/docs/drivers/postgres/) can follow a `LISTEN` channel and export results
with the server's own `COPY`. [Greenplum](/docs/drivers/greenplum/) has both;
[CockroachDB](/docs/drivers/cockroachdb/) has the export only; [Amazon Redshift](/docs/drivers/redshift/)
has neither.

## Following a channel

PostgreSQL has no change streams, so a live tail follows a `LISTEN` channel. Every `NOTIFY` on
that channel arrives as an event with the op `notify`, the channel name and the payload.

```sh
tobira watch app orders_changed
tobira watch app orders_changed --format table --max 10
```

In the terminal UI, `:watch orders_changed` opens a [live tail](/docs/tui/#live-tail) tab. See
also [`tobira watch`](/docs/cli/watch/).

- tobira starts listening before the command returns, so a `NOTIFY` sent right after is not
  missed.
- The channel name keeps its case and punctuation. A name longer than 63 bytes is refused,
  because the server would cut it.
- Watching never writes, so it works on read-only connections.
- If the server connection is lost, the watch ends with that error.
- A watch holds one connection of its own for as long as it runs.

## Native csv export

`tobira query --output file.csv` with no `--limit` lets the server write the csv with
`COPY (<query>) TO STDOUT`. The values are then PostgreSQL's own text (booleans as `t` and `f`),
and the file is streamed in constant memory. See [Export](/docs/export/) for the general rules.

```sh
tobira query app "select * from orders" --output orders.csv
```

The native export only takes one `SELECT`, `WITH`, `VALUES` or `TABLE` statement, without bind
parameters or a row limit (put `LIMIT` in the query). It always runs in a read-only transaction
that is rolled back, on writable connections too. When it refuses a query, tobira writes the
file with its own csv writer instead:

- on a writable connection, a query that writes (`nextval()`, a data-modifying `WITH`) falls back
  to tobira's csv;
- on a read-only connection, such a query stays refused.

Set `--limit` to always use tobira's csv.

The export also refuses everything the [read-only guard](/docs/drivers/postgres/read-only/)
refuses (`COPY`, `DO`, server-file functions), on every connection, and it checks that the query
text cannot break out of the `COPY ( ... )` around it.
