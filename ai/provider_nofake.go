//go:build !aifake

package ai

// fakeProvider reports no provider: the scripted test provider exists only
// in binaries built with -tags aifake (see provider_fake.go).
func fakeProvider(string) (provider, bool) { return provider{}, false }
