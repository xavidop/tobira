// Package bigquery is the tobira driver for Google BigQuery, built on
// cloud.google.com/go/bigquery over the REST API. Importing it registers
// the "bigquery" driver in driver.Default for the bigquery scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/bigquery/
//
// Read-only is a hard guard in three layers: a fail-closed GoogleSQL
// allow-list checked before any text is sent (guard.go, lex.go), a dry run
// whose statementType must be SELECT, and maximumBytesBilled on every job.
// Credentials come only from ConnConfig.Password (a service-account key)
// or Application Default Credentials, never from the DSN, and are masked
// out of every error.
package bigquery
