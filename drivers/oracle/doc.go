// Package oracle is the tobira driver for Oracle Database, built on the
// pure-Go github.com/sijms/go-ora/v2 through database/sql. Importing it
// registers the "oracle" driver in driver.Default for the oracle scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/oracle/
//
// Read-only is a hard guard in two layers: a fail-closed allow-list checked
// before any text is sent (guard.go, lexed with driverutil/sqlscan's
// ScanOracle), then SET TRANSACTION READ ONLY on a dedicated session, with a
// check after the statement that the transaction is still the one begun
// (DBMS_TRANSACTION.LOCAL_TRANSACTION_ID), and a ROLLBACK. Oracle commits
// implicitly before DDL, so the allow-list is what keeps DDL, PL/SQL and
// procedure calls away from the server; the transaction id check catches
// anything that slips past it, after the fact, and discards the session.
package oracle
