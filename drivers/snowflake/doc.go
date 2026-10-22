// Package snowflake is the tobira driver for Snowflake, built on
// github.com/snowflakedb/gosnowflake/v2 over database/sql. Importing it
// registers the "snowflake" driver in driver.Default for the snowflake
// scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/snowflake/
//
// Read-only is a hard, fail-closed lexical guard (guard.go) checked before
// any text is sent: one SELECT, WITH, SHOW, DESCRIBE or EXPLAIN statement,
// with no write, procedure call, stage reference, flow operator or
// side-effecting SYSTEM$ function anywhere in it. The session and every
// request also carry MULTI_STATEMENT_COUNT=1. Snowflake has no read-only
// session or transaction mode to add underneath; a role with only SELECT
// grants is the server-side layer a user can add.
//
// gosnowflake logs through a process-wide logger; init replaces it with
// one that discards everything, so the SDK never writes to the terminal
// or, through a client config file, to a log directory.
package snowflake
