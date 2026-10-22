// Package vertica is the tobira driver for Vertica (OpenText Analytics
// Database), built on github.com/vertica/vertica-sql-go over database/sql.
// Importing it registers the "vertica" driver in driver.Default for the
// vertica scheme.
//
// Read-only has three layers (guard.go): a fail-closed lexical allow-list
// checked before any text is sent (one SELECT, WITH, SHOW or EXPLAIN
// statement, no write or session word, and only allow-listed built-in
// functions, since Vertica's meta-functions act from a SELECT); START
// TRANSACTION READ ONLY around every read-only call, always rolled back,
// plus READ ONLY session characteristics on every session of a read-only
// connection; and, on every path, one statement as both the guard and
// vertica-sql-go's client-side splitter count them, with COPY FROM LOCAL
// and FROM STDIN refused.
//
// vertica-sql-go logs to stdout and can exit the process on a FATAL
// message; init silences it.
package vertica
