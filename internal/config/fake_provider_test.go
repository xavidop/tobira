//go:build !aifake

package config

import (
	"strings"
	"testing"
)

// The scripted fake provider exists only in binaries built with -tags
// aifake; anywhere else ai.provider: fake is a validation error.
func TestAIFakeProviderIsRefusedWithoutTag(t *testing.T) {
	_, err := Load(writeTemp(t, "ai:\n  provider: fake\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("provider fake: err = %v, want unknown provider", err)
	}
	if AIFakeProvider() != "" {
		t.Fatalf("AIFakeProvider = %q in a normal build", AIFakeProvider())
	}
}
