// Package influxdb is the tobira driver for InfluxDB 3, built on
// github.com/InfluxCommunity/influxdb3-go over Arrow Flight. Importing it
// registers the "influxdb" driver in driver.Default for the influxdb3 and
// influxdb schemes.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/influxdb/
//
// InfluxDB 3's query API cannot write: it plans no DML or DDL. On top of
// that the driver enforces a fail-closed allow-list (guard.go) on every
// statement it sends, so only SELECT, SHOW and EXPLAIN of a SELECT reach
// the server.
package influxdb
