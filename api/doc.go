// Package api is tobira's stable surface for driver plugins and embedders.
//
// Every type here is an alias of the same type in package driver
// (type Value = driver.Value, and so on), so there is one source of truth
// and a value moves between the two packages with no conversion. The
// constants, sentinel errors and functions forward to driver as well.
//
// The stability promise is about this package: within a major version of
// the plugin protocol (tobira.plugin.v1), nothing exported from api is
// removed or changes meaning. New names may be added, mirroring new
// optional capabilities in driver. Package driver remains where the model
// is defined and documented, and may grow helpers that api does not
// re-export; code that limits itself to api is insulated from those.
//
// What api deliberately leaves out: the process-wide registry (Default,
// Register, Open, OpenConfig, Drivers), which belongs to the host. A plugin
// never registers itself; it calls plugin.Serve from the plugin/sdk module
// with its Driver, and the host registers a proxy for it.
//
// api imports only package driver, which imports only the standard library.
package api
