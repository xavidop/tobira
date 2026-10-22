// Package convert translates between the driver types and the plugin
// protocol messages in plugin/proto/tobira/plugin/v1. Both sides of the
// plugin boundary use it: the SDK (plugin/sdk) to encode what a driver
// returns, and the host (plugin/host) to decode what a plugin sends.
//
// Every decode validates before it allocates a driver value: a plugin is
// untrusted input. The limits are MaxValueBytes per value, MaxColumns per
// result, MaxNameBytes per name (column, object key, native type, zone) and
// MaxDepth levels of list/object nesting. A message that breaks a limit is an
// ErrLimit; a malformed one (an unset oneof, a nil element, a row of the
// wrong width, frames out of order) is an ErrInvalid. Nothing here panics on
// bad input. The encoders enforce the same limits, so a plugin learns about
// an oversized value from its own SDK rather than from a host rejection.
//
// Result sets travel as ResultFrame streams: SendResultSet writes one and
// NewFrameResultSet reads one back as a driver.MultiResultSet, cancelling a
// blocked receive when the caller's context ends.
//
// GRPCDialOptions and GRPCServerOptions set both sides' message caps to
// MaxMessageBytes; the host and the SDK must pass them.
package convert
