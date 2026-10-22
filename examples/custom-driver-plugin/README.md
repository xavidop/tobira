# custom-driver-plugin

An example **external driver plugin** for tobira: a small in-memory driver (`memdemo`) built
with the Go SDK and served as its own process over tobira's plugin protocol. Unlike
[`custom-driver-module`](../custom-driver-module), it is not compiled into tobira. You build a
binary named `tobira-driver-memdemo` and drop it in tobira's plugins directory.

The whole plugin is one line of `main`:

```go
func main() { sdk.Serve(memdemo.New()) }
```

The driver lives in [`memdemo`](memdemo). It holds one namespace, `main`, with the conformance
kit's People collection, `tobira_people`. It offers browse (filters, sort, paging), editing
(insert, update and delete by key), explain, snapshot transactions and completion, and it
declares the read-only guarantee. The data lives in the plugin process, so every tobira run
starts with fresh data.

## Build

```sh
cd examples/custom-driver-plugin
go build -o tobira-driver-memdemo .
```

The binary name matters: tobira only loads files named `tobira-driver-<name>` (lowercase
letters, digits and dashes), and `<name>`, here `memdemo`, becomes the driver name.

## Install

Use `tobira driver install`, which requires the binary's sha256 and puts it in the plugins directory,
`$XDG_CONFIG_HOME/tobira/plugins/` (`~/.config/tobira/plugins/` when `XDG_CONFIG_HOME` is unset),
created with mode 0700:

```sh
sha256sum tobira-driver-memdemo      # shasum -a 256 on macOS
tobira driver install ./tobira-driver-memdemo --sha256 <the 64 hex characters>
tobira driver list                   # memdemo, source plugin:<path>
tobira driver info memdemo           # starts it and shows its capabilities
```

`install` never runs the binary. Copying the file into the directory yourself works too.

tobira also finds plugins on `PATH`, or in the directories listed under `plugins.dirs` in the
config. It refuses a binary that is world-writable or sits in a world-writable directory
(unless that directory is sticky, like `/tmp`). If you rebuild the plugin while tobira is
running, restart tobira so it uses the new binary.

## Use

Add a connection whose DSN uses the plugin's scheme:

```yaml
connections:
  - name: demo
    dsn: memdemo://
```

Then use it like any other connection: `tobira connect demo` opens the TUI, and on the command line:

```sh
tobira query demo "select * from tobira_people where id = 7"
tobira query demo "select * from tobira_people where name = 'Ada Lovelace'" --format json
tobira query demo "select * from tobira_people where active = true" --explain
```

The query language:

```text
select * from <collection> [where <field> = <value>]
delete from <collection> [where <field> = <value>]
reset <collection>
sleep <duration>
```

A value is an integer, a decimal number, a `'quoted string'` (a quote inside it is written twice, as in `'O''Brien'`), `true`,
`false`, `null` or a parameter `$1`. `reset` puts the collection back to its seed data. `sleep 10s` waits up to 60s and returns no
rows; cancelling it (Ctrl-C, or a timeout) ends it at once, which is what the conformance kit
checks.
Connections with `read_only: true` refuse `delete` and `reset`.

## Tests

`go test ./...` runs:

- the `drivertest` conformance kit in Strict mode against the driver in process;
- the same suite, with the same fixture, through tobira's plugin host. The test builds the
  plugin into a temp directory, discovers and registers it as tobira does at startup, and runs
  every call across the protocol into the plugin process. It also checks that no goroutine is
  left behind once the host closes;
- an end-to-end test that builds `cmd/tobira` and the plugin, points `plugins.dirs` at the
  plugin, and checks the rows `tobira query` prints.

A plugin is a program you run as yourself: tobira gives it an environment allow-list, `/dev/null` as stdin
and the connection's password only when a connection opens. The trust
model and its limits are on the [security page](https://xavidop.github.io/tobira/docs/security/),
plugin authoring is in [Write a driver](https://xavidop.github.io/tobira/docs/write-a-driver/), and
the protocol and the SDK are on the [Plugin protocol](https://xavidop.github.io/tobira/docs/plugin-protocol/) page.
