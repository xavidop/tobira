---
layout: ../../../layouts/DocsLayout.astro
title: Driver module
---

# Write a driver module

A built-in driver is a Go module that implements the `driver` contract and registers itself. This
page walks through the steps; [The conformance kit](/docs/write-a-driver/conformance/) tests the
result.

The runnable example is
[`examples/custom-driver-module`](https://github.com/xavidop/tobira/tree/main/examples/custom-driver-module):
a map-backed driver of about 150 lines that passes the kit in strict mode. Read it next to this page.

## 1. Make a module

One module per driver keeps a database SDK out of everyone who does not use it. Require
`github.com/xavidop/tobira` for the `driver` package and, in tests, `drivertest`. The root module
requires no database SDK, and neither does `driver` (it imports only the standard library).

## 2. Implement Driver and Conn

```go
type Driver interface {
    Info() driver.Info
    Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error)
}
```

`Info` is static: a name, a kind, DSN schemes, the dialect name and the list of capabilities your
connections offer. `Open` builds a `driver.Conn`:

```go
type Conn interface {
    Query(ctx context.Context, q driver.Query) (driver.ResultSet, error)
    Ping(ctx context.Context) error
    Close() error
    Namespaces(ctx context.Context) ([]driver.Namespace, error)
    Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error)
    Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error)
}
```

Rules the kit checks:

- Every call takes a `ctx` and honours it. Cancellation errors satisfy
  `errors.Is(err, context.Canceled)`.
- Every error is a `*driver.Error` built with `driver.Wrap(kind, driverName, op, err)`, where `kind`
  is a sentinel such as `driver.ErrNotFound`, `ErrSyntax`, `ErrReadOnly` or `ErrConnection`.
- Read-only is a hard guard. `ConnConfig.ReadOnly` and `Query.ReadOnly` must make a write fail with
  `ErrReadOnly`, and the query text must not be able to switch the guard off.
- `Query.Limit` stops reading early and sets `Stats.Truncated`.
- `Describe` reports each field's flags: `Nullable`, `Default`, `Generated` (computed, never
  written) and `AutoIncrement` (the database assigns it when an insert leaves it out: serial,
  identity, `AUTO_INCREMENT`, a rowid). The TUI's inline insert never asks for a field that is
  nullable, has a default, is generated or auto-increments. Set `Fixture.AutoIncrementKey` to have
  the kit check what you report for the People key.
- `Close` is idempotent, and using a closed `Conn` returns an error.
- Values are the `driver.Value` tagged union (`driver.Int`, `driver.String`, `driver.Null` ...).
  Passwords arrive as `driver.Secret`; print DSNs only through `driver.RedactDSN`.
- Never write to stderr or the default `slog` logger: the TUI owns the terminal. Log warnings
  through `cfg.Log()`, which is `ConnConfig.Logger` (the host's `--debug` log) or a discarding
  logger when it is nil, and silence any process-wide logger your SDK writes to stderr.
  `drivertest.Stderr` captures file descriptor 2 so a test can prove a failed dial prints nothing.

## 3. Register in init

```go
func init() { driver.Register(New()) }
```

`Register` adds the driver to `driver.Default`, by name and by DSN scheme, and panics on a duplicate,
like `database/sql`. A program then enables the driver with a blank import, which is how `tobira`
enables its built-in drivers.

## 4. Add capabilities

Anything beyond the core is an optional interface your `Conn` also implements: `Browser`,
`Explainer`, and the rest. List each one in `Info().Capabilities`. Callers discover them with
`driver.As`:

```go
if b, ok := driver.As[driver.Browser](conn); ok {
    // the connection can page through a collection
}
```

A `Tx` may implement capabilities too, found the same way with
`driver.As[driver.Mutator](tx)` or `driver.As[driver.Explainer](tx)`: the TUI edits and explains
through an open transaction only when its `Tx` offers them, and the conformance kit checks a `Tx`
that is an `Explainer`. No built-in driver implements the graph reader, and its shape may change.

### Text similarity

When a `VectorQuery` has an empty `Vector` and a `Text`, your driver embeds the text itself with
`ConnConfig.Embedder` (or lets the database embed it, if it embeds natively). With neither, return
`ErrUnsupported` whose message is `driver.NeedEmbedderHint`, so the user is told to configure an
embedder.

### The read-only guarantee

One capability is a declared guarantee rather than an interface: `driver.CapReadOnly`
(`"readonly"`). List it in `Info().Capabilities` when your driver enforces `Query.ReadOnly` (and
`BrowseOptions.ReadOnly`) as a hard guard, so a write sent with the flag fails with
`driver.ErrReadOnly` and changes nothing, whatever the query text says: compound forms
(several statements, a writable CTE) included. The guarantee covers `Explainer.Explain` too:
explaining a write with `ReadOnly` never runs it, and an `ANALYZE` of a write fails with
`ErrReadOnly` (or `ErrUnsupported` when the driver has no `ANALYZE`). `driver.As` and
`CapabilitiesOf` do not see it; callers check it with `driver.HasCapability(info, driver.CapReadOnly)`.
tobira's AI tools run `query`, `sample` and `explain` only on connections whose driver declares it,
and offer the schema tools alone otherwise. When you declare it, the conformance kit's
`ReadOnlyGuard` subtest sends `Fixture.Write`, `Fixture.TxWrite` and each of
`Fixture.CompoundWrites` (the compound writes your dialect allows) with `ReadOnly` set and checks
that each fails with `ErrReadOnly` and leaves the data unchanged. If your connection is an
`Explainer`, it also explains `Fixture.Write` with and without `ANALYZE` and checks nothing
changed. Every built-in driver except SingleStore declares it.

### Catalog caches

A `Completer` that caches catalog lookups should also implement `driver.CatalogInvalidator`
(`Invalidate()`). It is optional and not a listed capability, so it does not go in
`Info().Capabilities`; the TUI finds it with `driver.As[driver.CatalogInvalidator](conn)` and calls
it after anything that may change the catalog: a DDL or DML editor run, a commit, an applied edit
and `:reload`. `driverutil/sqlcomplete.Cache` implements it.

### Marking a truncated preview

When a row holds a shortened preview of a value rather than the whole value (the first 4 KiB of a
string, or the first 100 fields of a hash), add a `Bool` column named `_truncated` right after the
column it describes. Set it to `true` when that row's preview was cut and `false` otherwise, and
list the field as `Generated` in `Describe` so nobody edits it. The TUI applies the rules that
follow only when `Describe` lists `_truncated` as `Generated`, so a real column with that name is
left alone. It hides the column from the browse grid (the row detail still shows it) and refuses to
edit a cut value ("value truncated in preview; edit it with a query"), because writing a preview
back would lose the rest. A collection with a column named `value` is guarded on that column too.
Values that fit are edited as usual. The Redis driver follows this convention.

## 5. Test it

Run the [conformance kit](/docs/write-a-driver/conformance/) in strict mode.

## Contributing a driver to tobira

A driver contributed to this repository has a few more requirements (CI, docs and test setup).
They are in [CONTRIBUTING.md](https://github.com/xavidop/tobira/blob/main/CONTRIBUTING.md#checklist-for-a-built-in-driver).
