// Package cli implements the tobira command-line interface.
//
// It builds a cobra command tree from injected build information and I/O
// streams; it never reads os.Args or calls os.Exit, so it is fully testable.
package cli
