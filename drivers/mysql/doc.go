// Package mysql is the tobira driver for MySQL, built on
// github.com/go-sql-driver/mysql through database/sql. Importing it registers
// the "mysql" driver in driver.Default for the mysql, mariadb and singlestore
// schemes. The server's flavor (MySQL, MariaDB or SingleStore) is detected at
// Open, or forced with flavor=, and selects the dialect (flavor.go).
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/mysql/
//
// Read-only is a hard guard in three layers: a fail-closed allow-list checked
// before any text is sent (guard.go), a START TRANSACTION READ ONLY … ROLLBACK
// on a dedicated connection with SET SESSION TRANSACTION READ ONLY, and a
// post-check that the statement did not leave that transaction. The
// connection is then discarded, so no session state survives the call
// (go-sql-driver's ResetSession only checks liveness; it does not reset the
// session).
package mysql
