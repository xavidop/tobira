// Package memdemo is a small in-memory tobira driver, served as an external
// plugin by the main package of this module (sdk.Serve(memdemo.New())).
//
// One driver value holds one store: a single namespace ("main") with the
// conformance kit's People collection ("tobira_people") in it. Every Conn
// the driver opens shares that store, so a write through one Conn is read
// through another, as in a real database. Run as a plugin, the store lives
// as long as the plugin process: tobira starts it on first use and stops it
// when tobira exits, so the data starts fresh with every tobira run.
//
// Its query language is deliberately tiny:
//
//	select * from <collection> [where <field> = <value>]
//	delete from <collection> [where <field> = <value>]
//	reset <collection>
//	sleep <duration>
//
// A value is an integer, a decimal number, a 'quoted string' (a quote
// inside it is written twice), true, false, null, or a positional
// parameter $1, $2… Keywords are case-insensitive and one trailing
// semicolon is allowed. reset puts the collection back to its seed data.
// sleep waits for a Go duration (at most 60s) and returns no rows, or
// ends early with context.Canceled when the caller cancels: it is the
// slow query the conformance kit cancels mid-flight.
//
// Beyond the core driver.Conn it offers browse (structured filter, sort and
// cursor paging), mutator (insert, update and delete by primary key),
// explainer, transactor (snapshot transactions that fail to commit with
// driver.ErrConflict when the store changed since Begin) and completer, and
// it declares the readonly guarantee. The tests of this module run the
// drivertest kit in Strict mode against it twice: in process, and as a
// plugin through tobira's plugin host.
package memdemo
