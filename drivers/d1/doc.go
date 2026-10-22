// Package d1 is the tobira driver for Cloudflare D1, reached through the D1
// REST API (POST …/accounts/{account}/d1/database/{database}/raw) with a
// stdlib-only HTTP client. The SQLite dialect logic (read-only guard, type
// mapping, catalog parsing, browse and mutation SQL) comes from
// driverutil/sqlitecore.
//
// DSN: d1://<account_id>/<database_id>[?endpoint=<api base url>], with the
// API token in the connection's password. There is no Transactor: the REST
// API runs each request on its own. The driver has been tested only against
// a local fake of the API, not against Cloudflare.
package d1
