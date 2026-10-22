// Package driver defines the model every tobira database driver implements,
// and the registry that finds drivers. It imports only the standard library.
//
// # Model
//
// A Driver describes itself with Info and opens connections from a ConnConfig.
// A Conn is the core contract: Query, Ping, Close, and the schema calls
// Namespaces, Collections and Describe. Queries return a streaming ResultSet
// whose columns are fixed up front; values are the Value tagged union, so core
// types carry no any, funcs or readers and can cross a plugin boundary.
// Failures are *Error values built with Wrap and classified by sentinels such
// as ErrNotFound, matched with errors.Is. Passwords travel as Secret, and DSNs
// are printed through RedactDSN.
//
// Context is the only cancellation and timeout mechanism: every call takes a
// context and honours it, and there is no Timeout field.
//
// # Registration
//
// Drivers register in a Registry by name and by DSN scheme. A driver package
// calls Register from init, which adds it to Default and panics on a duplicate,
// like database/sql. Open and OpenConfig pick the driver named by
// ConnConfig.Driver or, failing that, the one for the DSN scheme, and return
// ErrUnknownDriver if none matches. Embedders and tests can build their own
// registry with NewRegistry instead of using Default.
//
// # Capabilities
//
// Anything beyond the core is an optional interface, discovered with As:
//
//	if b, ok := driver.As[driver.Browser](conn); ok { ... }
//
// A Conn that also implements Supporter (typically a plugin proxy) can report
// that a statically satisfied interface is not available at runtime; As
// honours that. CapabilitiesOf lists what a value offers. A Tx may be asked for
// Mutator the same way. Adding an optional interface is non-breaking; changing
// a core interface is breaking, so the core stays minimal.
//
// # Stability
//
// The package is pre-1.0. Browser is implemented by every shipped driver.
// Transactor is implemented by the SQLite, PostgreSQL and MySQL drivers;
// Explainer, Mutator and Completer by those and some of the others;
// Exporter by PostgreSQL and ClickHouse; Watcher by PostgreSQL, MongoDB and
// Redis; VectorSearcher by Qdrant and PostgreSQL (pgvector); and
// TimeSeriesReader by InfluxDB. GraphReader is experimental: no real driver
// implements it yet, and its shape will be confirmed or changed when the
// first one does.
package driver
