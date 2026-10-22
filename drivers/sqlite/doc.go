// Package sqlite is the tobira driver for SQLite, built on the pure-Go
// modernc.org/sqlite (no cgo). Importing it registers the "sqlite" driver in
// driver.Default.
//
// Every connection owns up to two database/sql handles on the same file: a
// read-write handle (omitted when ConnConfig.ReadOnly) and a read-only handle
// opened with mode=ro and PRAGMA query_only. Read-only queries always use the
// read-only handle, so query text cannot lift the guard.
package sqlite
