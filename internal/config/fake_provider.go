//go:build aifake

package config

// AIFakeProvider is the scripted test provider, "fake". It is accepted only
// in binaries built with -tags aifake, for the end-to-end smoke test; see
// the ai module's aitest package.
func AIFakeProvider() string { return "fake" }
