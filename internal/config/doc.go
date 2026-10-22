// Package config loads the tobira connection list from the YAML config file
// (~/.config/tobira/config.yaml by default).
//
// The top-level "connections" and "tui" keys are read; other top-level keys
// (ai, mcp, history) are accepted and ignored. Keys inside a connection entry
// are checked strictly so a typo such as "pasword" is an error rather than a
// silently dropped secret.
//
// A connection entry that fails validation does not fail the load: it is
// recorded in Config.Broken (with its line, an error text that never holds a
// value, and the fields that parsed safely) and the other entries load. Only
// unparseable YAML or a bad tui section is a load error. The package depends
// on no UI library.
package config
