---
layout: ../../layouts/DocsLayout.astro
title: Write a driver
---

# Write a driver

A tobira driver teaches tobira one database. It is Go code that implements the `driver` contract,
tested with the `drivertest` conformance kit. You can ship it two ways: as a Go module compiled into
the tobira binary, or as an external plugin, a separate `tobira-driver-<name>` program that tobira
starts and talks to over gRPC. Each built-in driver is its own Go module, so a database SDK reaches
a build only when that driver is used.

## Quick start

Start from one of the runnable examples:

- [`examples/custom-driver-module`](https://github.com/xavidop/tobira/tree/main/examples/custom-driver-module):
  a map-backed driver of about 150 lines that passes the kit in strict mode.
- [`examples/custom-driver-plugin`](https://github.com/xavidop/tobira/tree/main/examples/custom-driver-plugin):
  `memdemo`, the same idea served as a plugin with one line of `main`.

```go
func init() { driver.Register(New()) }        // a driver module

func main() { sdk.Serve(memdemo.New()) }       // a plugin
```

## In this section

| Page | Covers |
|---|---|
| [Driver module](/docs/write-a-driver/module/) | Implementing `Driver` and `Conn`, registering, adding capabilities, and the checklist for a built-in driver |
| [Conformance kit](/docs/write-a-driver/conformance/) | Running `drivertest`, kind fixtures, storage limits and the other fixture options |
| [External plugin](/docs/write-a-driver/plugin/) | Serving, testing, installing and using a plugin, its trust model and limits |
| [Plugin protocol](/docs/plugin-protocol/) | The stable `api` package, the Go SDK, the host and the wire protocol |

## Which route to choose

| | Driver module | External plugin |
|---|---|---|
| Ships in | the tobira binary | its own binary |
| Change to tobira needed | yes (a blank import in `cmd/tobira`) | none |
| Languages | Go | Go, or any language that speaks gRPC |
| cgo | no (tobira's releases are built without cgo) | yes |
| Install | comes with tobira | `tobira driver install <url\|path> --sha256 <hex>` |

## External plugin

An external plugin is a `tobira-driver-<name>` binary in the plugins directory or on `PATH`. tobira
starts it when a connection first needs it. To the terminal UI, the CLI, the AI assistant and the
MCP server it is one more driver: a connection with `dsn: <name>://` uses it.

```sh
go build -o tobira-driver-memdemo .
# Linux:
tobira driver install ./tobira-driver-memdemo --sha256 "$(sha256sum tobira-driver-memdemo | cut -d' ' -f1)"
# macOS:
tobira driver install ./tobira-driver-memdemo --sha256 "$(shasum -a 256 tobira-driver-memdemo | cut -d' ' -f1)"
tobira driver info memdemo
```

`tobira driver install` requires the binary's sha256 and never runs the binary. The steps, the
trust model and the limits are on [External plugin](/docs/write-a-driver/plugin/).
