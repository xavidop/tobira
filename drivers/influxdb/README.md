# tobira InfluxDB 3 driver

The InfluxDB 3 (Core and Enterprise) driver for
[tobira](https://github.com/xavidop/tobira), built on the official InfluxDB 3
Go client (`github.com/InfluxCommunity/influxdb3-go/v2`). Queries travel over
Arrow Flight. It registers the `influxdb` driver for the `influxdb3://` and
`influxdb://` schemes and is compiled into the `tobira` binary.

## Connect

```text
influxdb3://127.0.0.1:8181/metrics
influxdb3://influx.example.com/metrics?language=influxql
influxdb3://10.0.0.5:8181/metrics?tls=false&allow_plaintext=1
```

The port defaults to 8181 and the path is the default database. The token
goes in the connection's `password:` reference (or the DSN's password
position, `influxdb3://:TOKEN@host`). Parameters: `language` (`sql` or
`influxql`), `tls` and `allow_plaintext`. TLS is on for any host that is not
this machine; plain HTTP to one needs `tls=false&allow_plaintext=1`, and
InfluxDB Cloud is always TLS.

## Quick example

```sh
tobira query "influxdb3://127.0.0.1:8181/metrics" \
  "SELECT * FROM cpu ORDER BY time DESC LIMIT 10"
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, tables, describe | yes; SQL or InfluxQL |
| Browse | yes; newest first |
| Time series | yes; raw points, or windows with mean, sum, min, max, count or last |
| Editing, transactions, explain, completion, native export, watching | no |

## Read-only

Every connection is read-only. InfluxDB 3's query API cannot write, and
tobira also refuses anything but one `SELECT`, `WITH … SELECT`, `SHOW` or
`EXPLAIN` of a query before it is sent.

## Compatibility

Tested with InfluxDB 3.6 Core, with authentication on.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/influxdb/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/influxdb"
```
