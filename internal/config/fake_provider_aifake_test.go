//go:build aifake

package config

import "testing"

// With -tags aifake the scripted fake provider is accepted (smoke tests).
func TestAIFakeProviderWithTag(t *testing.T) {
	c, err := Load(writeTemp(t, "ai:\n  provider: fake\n"))
	if err != nil || c.AI.Provider != "fake" {
		t.Fatalf("provider fake: %+v, %v", c, err)
	}
}
