// Package sshtunnel reaches a database through an SSH server: it opens an
// SSH session, listens on a loopback port and forwards each connection to
// the host and port the DSN names, as seen from the SSH server. The driver
// then connects to the loopback port, so no driver knows about SSH.
//
// Host keys are checked against a known_hosts file and never accepted on
// first use: an unknown host is an error that says how to add it.
package sshtunnel
