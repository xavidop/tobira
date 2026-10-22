//go:build !aifake

package config

// AIFakeProvider is "" in a normal build: the scripted test provider does
// not exist, and ai.provider: fake is an unknown provider. Builds with
// -tags aifake return "fake".
func AIFakeProvider() string { return "" }
