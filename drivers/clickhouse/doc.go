// Package clickhouse is the tobira driver for ClickHouse, built on
// github.com/ClickHouse/clickhouse-go/v2 (native protocol by default, HTTP on
// request). Importing it registers the "clickhouse" driver in driver.Default
// for the clickhouse scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/clickhouse/
//
// Read-only is a hard guard in two layers: a fail-closed statement
// allow-list checked before any text is sent (guard.go), and the server's
// readonly mode: the setting readonly=1 sent with every read-only call, or
// the user's own readonly profile setting, which Open probes. The server then
// refuses writes and DDL, and under readonly=1 the table functions that reach
// outside it. readonly=2 allows table functions, so for a user whose profile
// sets readonly the driver checks them itself: the server parses each
// read-only text (EXPLAIN AST) and only a few row generators (numbers,
// zeros, generateRandom, values, null) may appear as table functions.
package clickhouse
