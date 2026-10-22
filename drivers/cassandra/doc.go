// Package cassandra is the tobira driver for Apache Cassandra and
// ScyllaDB, built on the Apache gocql driver
// (github.com/apache/cassandra-gocql-driver/v2). Importing it registers the
// "cassandra" driver in driver.Default for the cassandra and scylla
// schemes.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/cassandra/
//
// Queries are CQL. Namespaces are keyspaces and collections are tables and
// materialized views; Describe reports each table's partition and
// clustering keys (driver.KindWideColumn). Browse pages with the server's
// paging state. Mutator writes one row by its whole primary key with
// lightweight transactions (IF NOT EXISTS, IF EXISTS). There is no
// Explainer and no Transactor: CQL has neither query plans nor
// transactions.
//
// Read-only is a hard guard: a fail-closed lexical allow-list (guard.go)
// of one SELECT or DESCRIBE statement calling built-in functions only.
package cassandra
