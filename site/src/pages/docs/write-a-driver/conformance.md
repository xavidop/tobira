---
layout: ../../../layouts/DocsLayout.astro
title: Conformance kit
---

# The conformance kit

`drivertest` is the conformance kit every driver must pass. It seeds a known dataset, runs the
driver's core calls and each capability it declares, and checks the results. Run it in strict mode,
in process for a module and through the plugin host for a plugin.

## Quick start

```go
func TestConformance(t *testing.T) {
    drivertest.Run(t, drivertest.Config{
        Driver:       New(),
        Open:         open(false),
        OpenReadOnly: open(true),
        Strict:       true,
        Fixture: drivertest.Fixture{
            Namespace:  "main",
            Collection: "tobira_people",
            SelectAll:  driver.Query{Text: "select * from tobira_people"},
            Missing:    driver.Query{Text: "select * from nope"},
            Write:      driver.Query{Text: "delete from tobira_people"},
        },
    })
}
```

`Open` must return a connection to a database already seeded with the kit's People dataset
(`drivertest.PeopleDataset()`; `drivertest.SQLSeed(dialect)` renders it as SQL statements). The
fixture holds queries in your driver's own language. `Strict: true` makes every capability you
claim mandatory instead of skipped. Every driver in this repository runs strict. The kit also ships a
`drivertest.Fake`, an in-memory driver useful for testing a host without a database.

## Kind fixtures

Beyond People, the kit has one fixture per kind of store. Seed the dataset into your database the
way your integration test seeds People, then point the matching `Fixture` field at it.

| Kind | Dataset | Strict fixture | What it checks |
|---|---|---|---|
| document | `Documents()` | (shape only) | nested objects, lists, mixed types, missing fields |
| keyvalue | `KeyValueDataset()` | (shape only) | string, hash, list, set and zset keys with TTLs |
| vector | `VectorDataset()` | `Vector: KindVectorFixture(ref)` | the nearest neighbour under each metric, `k` honoured |
| timeseries | `TimeSeriesDataset()` | `TimeSeries: KindTimeSeriesFixture(ref)` | the exact window aggregates |
| widecolumn | `WideColumnDataset()` | `WideColumn: KindWideColumnFixture(ref, partition)` | `PartitionKey`, `ClusteringKey` and `PrimaryKey` from Describe, and one partition read back in clustering order |
| search | `SearchDocuments()` | `Search: KindSearchFixture(ref, query)` | the index type and mapping, and full-text hits for `SearchTerm` ranked by a positive `_score` |

The wide-column and search subtests need no capability, so they run whenever their fixture is set.
Under `Strict`, a driver whose `Info().Kind` is `driver.KindWideColumn` or `driver.KindSearch` fails
until it sets its fixture. The two take a native query because only the driver knows its language:

```go
WideColumn: drivertest.KindWideColumnFixture(
    driver.CollectionRef{Namespace: ks, Name: drivertest.WideColumnCollection},
    driver.Query{Text: "SELECT * FROM events WHERE tenant = 'acme'"}), // no ORDER BY
Search: drivertest.KindSearchFixture(
    driver.CollectionRef{Name: drivertest.SearchCollection},
    driver.Query{Text: `{"index":"articles","query":{"multi_match":{"query":"database","fields":["title","body"]}}}`}),
```

A wide-column driver reports the partition columns in `CollectionSchema.PartitionKey`, the
clustering columns with their order in `ClusteringKey`, and the whole key in `PrimaryKey`
(partition first). The TUI then marks those columns `[P]`, `[C↓]` and `[C↑]` in the grid header,
and the AI schema context lists them. A search driver lists its indexes as
`driver.CollectionIndex`, describes the mapping as (nested) fields, and returns a numeric `_score`
column, which the TUI shows with three decimals. `drivertest.NewFake(drivertest.WithWideColumns(),
drivertest.WithSearch(), drivertest.WithKind(kind))` seeds both fixtures in the Fake, for testing a
host.

## Declared storage limits

`Fixture.TypeOverrides` records how your database stores a People column when it has no native
type for it. Describe must then report the override type, and the data is still compared, by
meaning. A key is a People column (`"active"`) or `"<collection>.<column>"`. Declare only real
limits; never use an override to hide a bug.

| The database has | Override | The value must be |
|---|---|---|
| no boolean (Oracle before 23ai, as `NUMBER(1)`) | `"active": driver.TypeInt` | 1 or 0 |
| no fixed decimal for `score` (InfluxDB, Firestore) | `"score": driver.TypeFloat` | within 1e-9 |
| one decimal number type (DynamoDB's `N`, an unconstrained Oracle `NUMBER`) | `"id": driver.TypeDecimal`, `"age": driver.TypeDecimal` | numerically equal |
| one float number type (a JSON store that reads every number as a float) | `"id": driver.TypeFloat`, `"age": driver.TypeFloat` | within 1e-9 |
| string keys only (Cosmos DB, whose `id` must be a string) | `"id": driver.TypeString` | the exact decimal text (`"7"`); the Browser and Mutator subtests then read and write ids as text |
| no JSON type (SQL Server's `nvarchar(max)`, Vertica) | `"tags": driver.TypeString` | a string holding equal JSON |
| JSON arrays as native lists (Cassandra `list<text>`, DynamoDB `L`, Firestore arrays) | `"tags": driver.TypeList` | a list equal to the JSON array |
| no time type | `"created_at": driver.TypeString` or `driver.TypeInt` | RFC 3339 text for the same instant, or Unix nanoseconds |

A store with no NULL/unset distinction (Cassandra) needs no override: People's NULL `age` reads
back as NULL. Overrides hold everywhere the kit compares People, including the Transactor's
commit check.

## Unordered reads

The kit compares `SelectAll` with People row by row, in id order, so write it with `ORDER BY id`
when your language can. A store that cannot read a whole collection in key order sets
`Fixture.UnorderedSelectAll: true` instead. DynamoDB is one: a scan comes back in hash order, and
PartiQL orders only within one partition. The kit then matches the rows to People by id before
comparing them. Every id must appear exactly once, and the row count, the columns and every value
are still checked, so a wrong, missing, extra or duplicated row still fails. Never sort the rows
yourself in a test wrapper: the kit must see what the driver returns.

## Insert defaults

The Mutator suite inserts a People-shaped probe row. A store that cannot hold a row without an
extra member sets `Fixture.InsertDefaults` instead of wrapping its connection. Pinecone is one:
every record needs its dense vector, so its conformance test sets
`InsertDefaults: map[string]driver.Value{"values": driver.Vector(...)}`. The kit appends those
members to the probe row, in key order, on every Insert it makes, including the read-only check. A
default may only add a member: one that names a People column fails the suite, so the probe's own
values are always what the later checks compare.
