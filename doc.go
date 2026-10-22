// Package tobira is the root of tobira, a terminal client for every database
// ("Every database, one terminal.").
//
// The root module never depends on a database SDK. Each database lives behind
// the driver model defined in the driver package, and concrete drivers ship as
// separate modules so that importing tobira does not pull in any client
// library. See package driver for the contract drivers implement and package drivertest for the
// conformance kit.
//
// The tobira binary (module cmd/tobira) wires the drivers, the terminal UI,
// the AI assistant and the MCP server together; this root module holds the
// driver contract, the shared helpers and the internal packages they use.
package tobira
