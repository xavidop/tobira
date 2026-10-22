// Package session manages the lazily opened database connections of one
// tobira run. It has no UI dependency: the TUI turns its results into
// messages. Broken config entries are listed as connections that are always
// Failed with their config error. A Manager is safe for concurrent use.
package session
