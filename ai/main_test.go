package ai

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
)

// netCalls counts requests that reached the default transport and DNS
// lookups that reached the resolver. No test may touch the network: plugin
// init must be local. What is guarded:
//   - The providers' REST clients (genai, anthropic, openai, ollama) and the
//     cloud auth transport use http.DefaultTransport, replaced here.
//   - Every name lookup goes through net.DefaultResolver, replaced here by a
//     pure-Go resolver whose DNS dial counts and fails. This covers clients
//     with their own transport, such as the GCE metadata client.
//   - GCE_METADATA_HOST points the metadata client at a .test name, so
//     metadata.OnGCE answers without probing 169.254.169.254, and any
//     metadata request needs a DNS lookup, which the resolver catches.
//
// Not guarded: a dial to a literal IP from a client with its own transport.
// None is reachable from these tests: the only such client is the metadata
// one, redirected above.
var netCalls atomic.Int64

type noNetwork struct{}

func (noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	netCalls.Add(1)
	return nil, errors.New("network disabled in tests: " + r.URL.Host)
}

func TestMain(m *testing.M) {
	// Genkit logs through slog's default; the binary discards it (main.go).
	slog.SetDefault(slog.New(slog.DiscardHandler))
	http.DefaultTransport = noNetwork{}
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
		netCalls.Add(1)
		return nil, errors.New("DNS disabled in tests: " + addr)
	}}
	_ = os.Setenv("GCE_METADATA_HOST", "metadata.tobira.test")
	// Genkit reads these itself; make sure the developer's own environment
	// cannot leak into or change a test.
	for _, k := range []string{
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"OPENAI_API_KEY", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION",
		"VERTEX_API_KEY", "GOOGLE_GENAI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GENKIT_ENV",
		"GENKIT_TELEMETRY_SERVER", "GENKIT_REFLECTION_V2_SERVER", "GENKIT_REFLECTION_PORT",
	} {
		_ = os.Unsetenv(k)
	}
	code := m.Run()
	if code == 0 && netCalls.Load() != 0 {
		println("tests made", netCalls.Load(), "network requests")
		code = 1
	}
	os.Exit(code)
}
