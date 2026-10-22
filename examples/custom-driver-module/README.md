# custom-driver-module

An example **driver module** for tobira: a tiny in-memory driver (`memdriver`), written to be
read. Start here when you want to add a database to tobira as Go code compiled into the binary.
To ship a driver as a separate program instead, see
[`custom-driver-plugin`](../custom-driver-plugin).

The driver serves one namespace, `main`, with one collection, `tobira_people`, copied from the
conformance kit's People dataset when a connection opens. The only query it understands is:

```sql
select * from <collection>
```

It has no optional capabilities, so it implements only `driver.Conn`. A real driver adds the
interfaces it supports, such as browsing, editing or explain.

## Files

| File | What it shows |
| --- | --- |
| [`memdriver.go`](memdriver.go) | The driver: `Info`, `Open`, and a `Conn` that queries, lists and describes |
| [`memdriver_test.go`](memdriver_test.go) | `drivertest.Run` in Strict mode, the bar every tobira driver has to clear |

## Run the test

```sh
cd examples/custom-driver-module
GOWORK=off go test ./...
```

## Learn more

The walkthrough is [Write a driver](https://xavidop.github.io/tobira/docs/write-a-driver/), with
[Driver module](https://xavidop.github.io/tobira/docs/write-a-driver/module/) and the
[conformance kit](https://xavidop.github.io/tobira/docs/write-a-driver/conformance/).
