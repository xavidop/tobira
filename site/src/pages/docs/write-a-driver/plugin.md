---
layout: ../../../layouts/DocsLayout.astro
title: External plugin
---

# External plugin

A plugin is a program named `tobira-driver-<name>` that tobira finds, starts when something first
needs it (see [Lazy `Info`](#limits)), and talks to over gRPC (through [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin)).
To the TUI, the CLI, the assistant and MCP it is one more driver. Use this route when the driver
should not live in this repository or tobira's binary, or when it is not written in Go.

The runnable example is
[`examples/custom-driver-plugin`](https://github.com/xavidop/tobira/tree/main/examples/custom-driver-plugin):
`memdemo`, an in-memory driver served by one line of `main`. The steps below use it.

## 1. Write the driver against `api`

A Go plugin is a `driver.Driver` like a [driver module](/docs/write-a-driver/module/), and the same rules apply (honour
`ctx`, return `*driver.Error`, list only the capabilities you implement). Import the stable
[`api`](/docs/plugin-protocol/#the-api-package) package instead of `driver` if you want a surface
that stays put across tobira releases: its types are aliases, so `api.Driver` *is* `driver.Driver`.
A plugin never calls `Register`; the host registers a proxy for it.

## 2. Serve it

```go
package main

import (
    "github.com/xavidop/tobira/examples/custom-driver-plugin/memdemo"
    "github.com/xavidop/tobira/plugin/sdk"
)

func main() { sdk.Serve(memdemo.New()) }
```

`plugin/sdk` is its own Go module. `sdk.Serve` does the handshake, serves the protocol, keeps
connections and transactions behind unguessable handles, redacts error messages, turns a panic in one
call into a failed call, and exits when tobira dies. Never write to stdout: go-plugin owns it. Log
through `cfg.Log()`; it goes to stderr, and `TOBIRA_PLUGIN_LOG_LEVEL` sets the level.

## 3. Test it

Run the [conformance kit](/docs/write-a-driver/conformance/) in Strict mode against the driver in
process. Then run
the same suite through tobira's plugin host, so the protocol is covered too: build the plugin into a
temp directory, discover and register it with `plugin/host`, and pass that driver to `drivertest.Run`.
`examples/custom-driver-plugin/conformance_test.go` does both with one fixture.

## 4. Build, install and check

The binary name is the driver name: `tobira-driver-memdemo` is the driver `memdemo`. A name matches
`^tobira-driver-[a-z0-9][a-z0-9-]{0,62}$`; anything else is ignored.

```sh
cd examples/custom-driver-plugin
go build -o tobira-driver-memdemo .
sha256sum tobira-driver-memdemo          # shasum -a 256 on macOS

tobira driver install ./tobira-driver-memdemo --sha256 <the 64 hex characters above>
tobira driver list                        # memdemo, source plugin:<path>
tobira driver info memdemo                # starts it: capabilities, path and sha256
```

`install` requires the checksum, whether the source is a URL or a file, and writes the binary into the
plugins directory (`$XDG_CONFIG_HOME/tobira/plugins/`, mode 0700) only if it matches. It never runs
the binary; `driver info` does. You can also copy the file there yourself, put it on `PATH` or list
its directory under `plugins.dirs`. See the [`tobira driver` reference](/docs/cli/driver/) and the
[configuration](/docs/configuration/#the-plugins-section).

## 5. Use it

```yaml
connections:
  - name: demo
    dsn: memdemo://
```

```sh
tobira query demo "select * from tobira_people where id = 7"
tobira connect demo
```

A plugin is registered under its name, and DSNs with the scheme `<name>://` go to it. Set `driver:
<name>` on a connection to reach it through another scheme.

## Reference

The wire protocol, the `api` package, the host's behaviour and the limits are on
[Plugin protocol](/docs/plugin-protocol/). The `.proto` file is
`plugin/proto/tobira/plugin/v1/driver.proto`, package `tobira.plugin.v1`, protobuf edition 2023. A
plugin in another language implements the `Driver` service from it, speaks go-plugin's handshake
(protocol version 1, the magic cookie `TOBIRA_PLUGIN_MAGIC`, gRPC) and must accept string fields that
are not valid UTF-8.

## Trust model

See also [Plugin trust](/docs/security/plugins/).

A plugin is a program you run as yourself. tobira cannot sandbox it, and a plugin you install can read
your files and use your network. Install only binaries you trust, from a source you can check. What
tobira does to limit the damage:

- **A required sha256.** `tobira driver install` refuses a binary whose checksum you did not give or
  that does not match, so a swapped download is caught. Writes are atomic and capped at 256 MiB.
- **Where it runs from.** A binary that is world-writable, or in a world-writable directory (a sticky
  one such as `/tmp` is allowed), is refused. Before every start, tobira checks that the file is the
  one it found at discovery (device, inode, mode and size) and refuses a swapped one until it
  restarts.
- **An environment allow-list.** A plugin gets `PATH`, `HOME`, `TMPDIR`, `LANG`, `LC_*`, `TZ` and
  `TOBIRA_PLUGIN_*` only (not names ending in `_PASSWORD`, `_TOKEN`, `_KEY` or `_SECRET`). The
  `TOBIRA_CONN_*` variables and your provider keys are never passed.
- **Secrets only in `Open`.** A connection's password travels in `Open`'s `ConnConfig`, over a local
  gRPC connection secured with go-plugin's AutoMTLS, and only when that connection opens.
- **Stdin and stdout.** Stdin is `/dev/null`, so a plugin cannot read your keystrokes or an MCP
  stream. Stdout is reserved for the handshake and never shown. Stderr goes to the `--debug` log,
  redacted, with control characters escaped.
- **Untrusted answers.** Everything a plugin returns is checked against size and depth limits before
  tobira uses it, and error text is redacted again host-side.
- **Cleanup.** A crashed plugin fails its calls, and the next `Open` starts a new process. Plugins are
  shut down when tobira exits, and a plugin built with the SDK exits if tobira is killed.

The read-only guard is the driver's job. A plugin that declares `readonly` is trusted to enforce it;
tobira cannot check that from outside, so install plugins you trust before you rely on it for
production.

## Limits

- **No host embedder.** A plugin gets no `ConnConfig.Embedder`. For a text similarity search the host
  embeds the text with the configured embedder and sends the vector along with the text.
- **No parent-death watch on Windows.** On Linux and macOS an SDK plugin exits within a second if tobira
  dies, and on Linux it also gets `SIGKILL` as its parent-death signal. On Windows neither exists, so a
  plugin can outlive a killed tobira.
- **`name://` routing only.** DSNs with the scheme `<name>://` go to the plugin. Another scheme the
  plugin lists in its `Info` is not routed automatically: set `driver:` on the connection.
- **Lazy `Info`.** tobira never starts a plugin just to list or validate drivers. Until the plugin
  has answered, its driver reports a stub: kind `other`, its name as its only scheme, no dialect and
  no capabilities.

  | Where | When the plugin starts | If it does not answer |
  |---|---|---|
  | A connection | On its first `Open` | The open fails |
  | `tobira driver list`, `driver info` | When run (`driver list --no-start` never starts it and shows `not started`) | The row says why |
  | `tobira mcp` | At start, for every exposed plugin connection, before the tools are built | The connection is listed as `unavailable` and every tool refuses it |
  | `tobira ask` | Before the model is called | `ask` exits 2 and says why |
  | The TUI sidebar and connection form | Never; they show the stub until the connection opens | — |
  | The TUI AI panel | At the start of each turn | The turn ends with the reason |

  `connections add` and `connections list` never start a plugin.
- **Panics.** A panic in a driver call fails that call. The driver's own state afterwards may be
  inconsistent, so keep calls free of half-done changes.
- **Restarts.** A restart gives a fresh process: connections that were open on the dead one stay dead.
