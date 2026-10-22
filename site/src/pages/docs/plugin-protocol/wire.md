---
layout: ../../../layouts/DocsLayout.astro
title: Wire protocol
---

# Wire protocol

The protocol is one file,
[`plugin/proto/tobira/plugin/v1/driver.proto`](https://github.com/xavidop/tobira/blob/main/plugin/proto/tobira/plugin/v1/driver.proto),
package `tobira.plugin.v1`. Its messages mirror the `driver` types one to one. The generated Go code
is committed next to it, so a Go plugin imports
`github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1` and needs no code generation.

## Service

One service, `Driver`, flattens `driver.Driver`, `Conn`, `Tx` and the capability interfaces.
`Open` returns an opaque `ConnHandle` and `Begin` a `TxHandle`; every later call carries the handle,
so one plugin process serves many connections.

| RPC | Go method | Capability |
| --- | --- | --- |
| `Info` | `Driver.Info` | |
| `Open`, `Close`, `Ping` | `Driver.Open`, `Conn.Close`, `Conn.Ping` | |
| `Namespaces`, `Collections`, `Describe` | the `Conn` catalog calls | |
| `Query` (stream) | `Conn.Query` | |
| `Browse` (stream) | `Browser.Browse` | `browser` |
| `Insert`, `Update`, `Delete` | `Mutator` | `mutator` |
| `Explain` | `Explainer.Explain` | `explainer` |
| `Begin`, `Commit`, `Rollback` | `Transactor.Begin`, `Tx.Commit`, `Tx.Rollback` | `transactor` |
| `TxQuery` (stream), `TxMutate`, `TxExplain` | `Tx.Query`, and `Mutator` / `Explainer` on a `Tx` | `transactor` |
| `Complete` | `Completer.Complete` | `completer` |
| `Invalidate` | `CatalogInvalidator.Invalidate` | `OpenResponse.catalog_invalidator` |
| `Similar` (stream) | `VectorSearcher.Similar` | `vector_searcher` |
| `Range` (stream) | `TimeSeriesReader.Range` | `timeseries_reader` |
| `Neighbors` | `GraphReader.Neighbors` (experimental) | `graph_reader` |
| `Export` (stream of bytes) | `Exporter.Export` | `exporter` |
| `Watch` (stream of events) | `Watcher.Watch` | `watcher` |

A plugin advertises its capabilities in `Info`, with the same strings as `driver.Capability`
(including the `readonly` guarantee). `OpenResponse.capabilities` narrows that list to what one
connection really offers, and `BeginResponse.capabilities` says whether a transaction offers
`mutator` or `explainer`. The host never calls an RPC whose capability was not advertised.
`Exporter.ExportFormats` travels in `OpenResponse.export_formats`, so it needs no call of its own.

## Values

`Value` is a `oneof` with one member per `driver.Type`, and a round trip is exact, bit for bit.

| `driver.Type` | Field | Notes |
| --- | --- | --- |
| null | `null` (empty message) | An unset oneof is an error, not NULL. |
| bool, int, float | `bool_value`, `int_value`, `float_value` | Floats keep their IEEE 754 bits, so NaN and ±Inf survive. |
| decimal | `decimal_value` (string) | The canonical text, e.g. `"12.50"`. |
| string, bytes | `string_value`, `bytes_value` | |
| time | `time_value` (`Time`) | A `google.protobuf.Timestamp` instant to the nanosecond, plus the zone name and offset. UTC and the local zone come back as `time.UTC` and `time.Local`; any other zone comes back as a fixed zone with the same abbreviation and offset, so the wall clock is unchanged. |
| json | `json_value` (string) | The raw text, kept even when it is not valid JSON. |
| vector | `vector_value` (`repeated float`) | |
| list | `list_value` (`repeated Value`) | |
| object | `object_value` (`repeated Member`) | A list, not a map, so key order and duplicate keys survive. |
| unknown | `unknown_value` | The native type name, the raw bytes and the display text. |

The file uses protobuf edition 2023 with proto3-style implicit presence and **no UTF-8
validation** on strings. Database text is not always valid UTF-8 (a Redis key, a latin1 column), and
a value must cross the boundary byte for byte. A plugin in another language must accept string
fields that are not valid UTF-8.

## Result streams

`Query`, `Browse`, `TxQuery`, `Similar` and `Range` return a stream of `ResultFrame`s: one `columns`
frame, then `rows` frames of at most 256 rows each, then one `stats` frame. A multi-statement result
repeats that sequence once per result set. A stream that ends before its `stats` frame is an error.
Cancellation travels as the call's gRPC context.

## Errors

A failed call returns a gRPC status whose details carry an `ErrorInfo`: the kind as
`driver.KindName` spells it (`not_found`, `read_only`, `syntax` and so on), the native error code,
the message, and the driver and operation. The host rebuilds a `*driver.Error` from it, so
`errors.Is(err, driver.ErrReadOnly)` works across the boundary. Messages are redacted with the
connection's config in the plugin and again in the host.

The status code is advisory; the host trusts the `ErrorInfo` kind. The Go SDK sends `NOT_FOUND` for
`not_found` and `unknown_driver`, `UNAUTHENTICATED` for `auth`, `PERMISSION_DENIED` for `permission`,
`FAILED_PRECONDITION` for `read_only` and `constraint`, `UNIMPLEMENTED` for `unsupported`,
`UNAVAILABLE` for `connection`, `INVALID_ARGUMENT` for `syntax`, `ABORTED` for `conflict`,
`RESOURCE_EXHAUSTED` for `limit`, and `CANCELLED` or `DEADLINE_EXCEEDED` when the call's context
ended. A malformed request is `INVALID_ARGUMENT` with an empty kind, and an oversized value
`RESOURCE_EXHAUSTED` with the kind `limit` (`driver.ErrLimit`, which the TUI and CLI show as a size
limit). `Close` of an
unknown connection and `Rollback` of an unknown transaction succeed, so `Rollback` after `Commit` is a
no-op. A `Rollback` or a second `Commit` sent while a `Commit` of the same transaction is still running
fails with `ABORTED`/`conflict`: that `Commit` may still succeed.

## Limits

gRPC messages are capped at 16 MiB in both directions; both sides raise gRPC's 4 MiB receive
default to that. On top of that the host rejects:

- a value whose encoding is over 1 MiB;
- a result with more than 4096 columns;
- any name (column, object key, native type, time zone) over 64 KiB;
- lists or objects nested more than 64 deep;
- more than 256 warnings or notices in a result's stats, any one of them over 64 KiB, or a next-page
  cursor over 64 KiB;
- a rows frame with no rows or more than 256, and frames out of order.

A plugin built with the Go SDK hits the same limits when it encodes, so it gets a clear error from its
own side. The SDK also keeps every frame inside one message: a row whose encoding is over 16 MiB less
1 KiB (20 cells of 900 KiB, say) cannot travel, so the SDK sends the rows before it and then ends the
result with the kind `limit`, which the host reports as `driver.ErrLimit`. The connection stays
usable.

The caps bound one message, not the memory it decodes to. A 16 MiB frame made of millions of tiny
list elements expands to several hundred MB of decoded values, which is inherent to protobuf. The
cap applies per frame, and frames are decoded one at a time.

## Compatibility

Field numbers are stable for the life of v1: a field is never renumbered, a number is never reused,
and a removed field becomes `reserved`. New fields take the next free number. An incompatible change
would be a new package, `tobira.plugin.v2`.

## Regenerating the Go code

If you change the proto, run `make proto`. It runs pinned versions of buf, protoc-gen-go and
protoc-gen-go-grpc with `go run`, so nothing needs installing and nothing is fetched from the Buf
Schema Registry. `make proto-check`, part of `make all` and of CI, fails when the committed code is
not what the proto generates.
