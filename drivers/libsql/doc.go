// Package libsql is the tobira driver for libSQL: sqld (libsql-server) and
// Turso. Importing it registers the "libsql" driver in driver.Default.
//
// It speaks Hrana over HTTP (the /v2/pipeline endpoint) with a small,
// stdlib-only client, so it needs no cgo. The SQL dialect is SQLite's: the
// read-only guard, catalog queries and type mapping mirror the sqlite
// driver's.
//
// sqld refuses PRAGMA query_only and ATTACH, so the read-only path cannot
// pin a session the way the sqlite driver does. Instead every read-only
// statement must pass the sqlite allow-list and then runs on a fresh stream
// inside BEGIN … ROLLBACK, in one batch whose statement step only runs when
// BEGIN succeeded: whatever it might change is rolled back on the server.
package libsql
