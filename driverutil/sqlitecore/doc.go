// Package sqlitecore is the SQLite dialect logic shared by the SQLite-family
// drivers (sqlite, libsql, d1): the read-only and single-statement guards
// with the PRAGMA allow-list, declared-type mapping and value conversion,
// rowid-alias detection from the CREATE TABLE text, catalog row parsing,
// mutation and browse SQL building, and EXPLAIN QUERY PLAN trees.
//
// It is standard library only (plus the root driver and sqlscan packages):
// no SQLite driver or other database SDK, so the root module stays free of
// them. Functions that return errors take the calling driver's registry
// name, which goes into each driver.Error.
package sqlitecore
