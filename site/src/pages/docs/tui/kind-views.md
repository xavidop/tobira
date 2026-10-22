---
layout: ../../../layouts/DocsLayout.astro
title: Kind views
---

# Kind views

The grid adapts to what a collection holds: documents, key-value pairs, wide-column rows, search
hits, time series or vectors. Each view works with any driver that offers what it needs, and
everything it shows is escaped like the rest of the terminal UI. The
[drivers matrix](/docs/drivers/) says which driver offers which view.

## Documents

- An object or array cell shows a one-line preview such as `{name: "Ada", tags: [2]}`: strings are
  quoted, a nested array shows its length and a nested object shows `{…}`. The preview is cut to
  the column width.
- Documents need not share their fields. The columns are every field seen so far, in the order
  first seen, and grow as you scroll into pages that bring new ones.
- A field a document does not have shows a dim `—`. A field set to null shows the italic `NULL`.
  Within one page a missing field can still read as `NULL`, depending on the driver.
- The row detail shows the document as a tree; `[` and `]` collapse or expand all of it.

## Key-value stores

A key-value collection shows `_key`, `type`, `ttl` and a value preview. The TTL is `∞` for no
expiry, or a duration such as `12s`, `1m30s`, `3h` or `2d1h`. A sorted set previews as
`{member: score, …}`.

The row detail lays the value out by type:

| Type | Shown as |
|---|---|
| string | its text, one line per line |
| hash | a field / value table |
| list | indexed lines |
| set | its members, sorted |
| zset | member and score |
| stream | each entry's id over its fields |
| json | a tree |

`e` on a value edits it like any [inline edit](/docs/editing/). A string opens as text and other
types open as JSON of their own type (an object for a hash, an array for a list). A stream cannot be
edited, because its entries are appended rather than replaced. If the driver cannot write a change,
`:apply` says so.

### Truncated previews

Some drivers show a shortened preview of a large value. Redis, for example, cuts a string at 4 KiB
and a hash, list, set, sorted set or stream at 100 elements. Such a value cannot be edited inline:
`e` answers "value truncated in preview; edit it with a query", because writing the preview back
would lose the rest. Use the [query editor](/docs/editor/) instead. The row detail shows a
`_truncated` column that says whether a value was cut.

## Wide-column tables

On a wide-column store such as Cassandra, the grid header marks the key once the table's schema is
read: `[P]` on the partition key columns, and `[C↓]` or `[C↑]` on the clustering columns, by their
clustering order. Rows show in the order the store returns them, which within a partition is the
clustering order.

## Search indexes

A search engine's index (Elasticsearch, OpenSearch) browses like a document collection, with the
same previews and tree detail. A numeric `_score` column, in a query result or in the grid, shows
the relevance score with three decimals (`1.288`), best hit first.

## Time series

`:chart <field> [window]` opens a chart of a numeric field of the open collection below the grid.
It works with any driver that can read time ranges.

- The chart covers the current time range, which is the last hour until you change it. `:range 6h`
  (or `30m`, `7d`) sets the last span up to now. `:range 2026-10-01..2026-10-02` sets fixed
  bounds; the end is exclusive, so that example is the whole of 1 October (UTC).
- Points are averaged in windows of the size you give (`5m`). Without one, tobira picks a round
  window of about one point per column.
- At 40 columns and wider the chart is an ASCII line chart, with the max and min on its axis and
  the time span, min, max and last value under it. On a narrower pane it is a sparkline
  (`▁▂▃▄▅▆▇█`).
- `ctrl+c` cancels a chart that is loading and `:chart off` closes it. Opening another collection
  closes it too.
- On a connection that cannot read time ranges, `:chart` says "charts need a time-series driver".

## Vectors

Similarity results carry a `_distance` column, with the nearest row first.

- **`S`** on a row searches for its 20 nearest neighbours, using the vector under the cursor or
  else the row's first vector. The results open in a new tab named `similar to <id>`, and `S` works
  again on that tab's rows.
- **`:similar <text>`** finds the rows nearest to a piece of text. The text is turned into a vector
  with the model set as `ai.embedder` (see [AI configuration](/docs/ai/configuration/)). Without
  one, the tab says "text similarity needs ai.embedder", followed by the reason when tobira knows
  it.
- A connection that cannot search vectors, or a row without a vector, gets a message instead.
- `ctrl+c` cancels a search like an editor run. Searches are not recorded in the query history.

## Live tail

`W` or `:watch` opens a live tail of the open collection in a new tab: its change events stream in
as they happen, newest first. It works with PostgreSQL, Greenplum, MongoDB, Redis, Amazon DynamoDB,
Cloud Firestore and Azure Cosmos DB. Watching never writes, so it works on read-only connections.

- `:watch <target>` watches a driver-specific target instead, such as a PostgreSQL `LISTEN` channel
  (required there, since PostgreSQL has no change streams), a MongoDB database, a Redis key pattern
  such as `session:*` or a Firestore document path. It uses the open collection's connection, or
  the selected connection when no collection is open. The table below lists each driver's targets.
- Each line shows the event's time, its operation (`op` in the table), its key (or channel) and a
  one-line preview of its payload. The title counts the
  events. The tab keeps the last 1000 and says so once older ones drop.
- Every event is redacted with the connection's secrets as it arrives, so the list and the detail
  never show the password. A payload over 64 KiB is cut and ends in `…(truncated)`; its detail then
  shows `_truncated: true`.
- `p` pauses and resumes. While paused, new events are still received and counted (`3 new`), but
  the list stays still. `c` clears the list.
- `enter` opens the event under the cursor in the [row detail](/docs/tui/browsing/#row-detail).
- `esc`, `x` or `:unwatch` closes the tab and stops the watch. `:watch` on a live tail tab replaces
  its watch; anywhere else it opens another tab.
- Opening a collection on another connection, a config change that may close the connection, and
  quitting stop the watch. The tab stays, marked stopped, with the events it got.
- A driver error ends the watch and shows, redacted, at the top of the tab. A connection whose
  driver cannot watch gets "live tail needs a driver with watch support".

### Targets by driver

| Driver | What it watches | Target | `op` values |
|---|---|---|---|
| PostgreSQL, Greenplum | A `LISTEN` channel. There are no change streams, so give a channel. | The channel name, such as `orders_changed` | `notify` |
| MongoDB | Change streams, on a replica set or a sharded cluster | `db.collection`, or a database name for all of its changes | MongoDB's operation type: `insert`, `update`, `replace`, `delete` and so on |
| Redis | Keyspace notifications, when the server has them enabled | A key pattern such as `session:*`, or a key prefix as `db0.user` | `insert` (new key), `update`, `delete` (also expired and evicted keys) |
| Amazon DynamoDB | DynamoDB Streams. The table needs a stream enabled. | The table name | `insert`, `update`, `delete` |
| Cloud Firestore | A snapshot listener | A collection path (`users`, `users/ada/orders`) or a document path (`users/ada`) | `insert`, `update`, `delete` |
| Azure Cosmos DB | The change feed, checked every second | A container name in the connection's database, or `database.container` | `upsert` for a created or replaced item. Deletes do not appear. |

From a script, [`tobira watch`](/docs/cli/watch/) streams the same events as JSON lines.
