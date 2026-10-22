// Package postgres is the tobira driver for PostgreSQL, built on
// github.com/jackc/pgx/v5 (pgxpool). Importing it registers the "postgres"
// driver in driver.Default for the postgres and postgresql schemes, and the
// flavored drivers "redshift", "cockroachdb" and "greenplum" for their own
// schemes. The postgres driver detects those flavors from the server unless
// the DSN forces one with flavor=… (see flavor.go).
//
// Read-only connections set default_transaction_read_only=on as a session
// runtime parameter (except on Redshift); the query layer adds further
// guards (spec 5.6), and CockroachDB and Redshift get a read-only
// allow-list (flavor_guard.go).
package postgres
