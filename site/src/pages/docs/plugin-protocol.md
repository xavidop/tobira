---
layout: ../../layouts/DocsLayout.astro
title: Plugin protocol
---

# Plugin protocol

A driver plugin is a separate `tobira-driver-<name>` binary that tobira starts and talks to over gRPC
through [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin). This section is the reference
for the pieces a plugin is built on: the **`api` package**, the stable Go surface a plugin is
written against; the **Go SDK**, which serves any driver with one call; and the
**wire protocol**, `tobira.plugin.v1`, which any language can implement.

[External plugin](/docs/write-a-driver/plugin/) walks through building one, and `tobira driver`
[lists, inspects and installs](/docs/cli/driver/) them.

## In this section

| Page | Covers |
|---|---|
| [The host](/docs/plugin-protocol/host/) | How tobira finds, starts and supervises plugins |
| [Wire protocol](/docs/plugin-protocol/wire/) | The gRPC service, values, result streams, errors, limits and compatibility |

## The `api` package

```go
import "github.com/xavidop/tobira/api"
```

`api` re-exports the whole public model of the `driver` package through type aliases:
`type Value = driver.Value`, `type Conn = driver.Conn`, and so on for every value, schema, result,
capability and error type. Constants, sentinel errors and helper functions (`api.Int`, `api.Wrap`,
`api.As`, `api.RedactMessage` and the rest) forward to `driver`. Because they are aliases, an
`api.Value` *is* a `driver.Value`: a driver written against `api` satisfies `driver.Driver` with no
conversion, and `errors.Is(err, api.ErrNotFound)` matches `driver.ErrNotFound`.

| | |
| --- | --- |
| Stability | Nothing exported from `api` is removed or changes meaning within protocol v1. New names may be added as `driver` gains optional capabilities. |
| Dependencies | `api` imports only `driver`, which imports only the standard library. |
| Left out | The process-wide registry (`driver.Default`, `Register`, `Open`, `OpenConfig`, `Drivers`). A plugin never registers itself; the host registers a proxy for it. |

## The Go SDK

```go
import "github.com/xavidop/tobira/plugin/sdk"

func main() { sdk.Serve(mydriver.New()) }
```

`plugin/sdk` is its own Go module. `sdk.Serve` performs the go-plugin handshake (protocol version 1,
the magic cookie `TOBIRA_PLUGIN_MAGIC`, gRPC only, AutoMTLS when the host asks for it) and serves the
protocol below on top of the driver:

- Conns and transactions live in the plugin behind random, unguessable handles. A call with an
  unknown handle fails with `NOT_FOUND`. Closing a connection rolls back its open transactions, and
  everything is released when the plugin shuts down.
- `Open` advertises what `driver.CapabilitiesOf` reports for the connection, and a call for a
  capability the connection (or transaction) lacks fails with `UNIMPLEMENTED`. Implement only the
  interfaces your database supports.
- Results stream as frames, and every value is checked against the limits below before it is sent.
- The host's cancellation reaches the driver through the call's context, mid-stream included.
  `Watch` sends its response headers once the driver's watch is established, so the host can wait
  for them before making the changes it expects to see.
- Error messages are redacted with the connection's config before they leave the plugin.
- A panic in a driver call fails only that call, with a generic `INTERNAL` "plugin panic" error; the
  details go to the plugin's stderr, redacted.
- If tobira dies without shutting the plugin down, the plugin notices within a second, releases its
  connections and transactions, and exits.

A plugin must never write to stdout, which go-plugin uses for the handshake. The
`ConnConfig.Logger` a driver receives writes JSON lines to stderr, and the host files them in its
debug log. Set `TOBIRA_PLUGIN_LOG_LEVEL` to `debug`, `info` (the default), `warn` or `error`.
