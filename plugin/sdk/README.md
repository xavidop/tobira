# plugin/sdk

The Go SDK for tobira driver plugins. It turns any `driver.Driver` into a `tobira-driver-<name>`
program that tobira starts and talks to over its plugin protocol. The whole `main` of a plugin is:

```go
package main

import "github.com/xavidop/tobira/plugin/sdk"

func main() { sdk.Serve(mydriver.New()) }
```

## What the SDK does for you

- Keeps connections and transactions in the plugin process, and releases them when tobira closes
  them or exits.
- Advertises the capabilities each connection really has, so you implement only the interfaces
  your database supports.
- Streams results, passes tobira's cancellation to your driver, and redacts every error message
  with the connection's credentials before it leaves the plugin.
- Turns a panic in a driver call into an error for that call only.
- Exits when tobira dies without shutting the plugin down.

A plugin must never write to stdout, which the protocol uses. Log through the `ConnConfig.Logger`
your driver receives; it writes to stderr, which tobira copies into its debug log.

## Run the tests

```sh
cd plugin/sdk
GOWORK=off go test ./...
```

## Learn more

- [External plugin](https://xavidop.github.io/tobira/docs/write-a-driver/plugin/): build, install
  and test a plugin.
- [`examples/custom-driver-plugin`](../../examples/custom-driver-plugin): a complete plugin.
- [Plugin protocol](https://xavidop.github.io/tobira/docs/plugin-protocol/): how tobira finds,
  starts and isolates plugins.
