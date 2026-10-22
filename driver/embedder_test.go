package driver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// keyEmbedder holds a credential, as a provider-backed embedder does.
type keyEmbedder struct{ key driver.Secret }

func (keyEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (keyEmbedder) Dimensions() int { return 2 }

// The host's embedder reaches Open through ConnConfig, untouched by the
// registry.
func TestConnConfigCarriesEmbedder(t *testing.T) {
	var got driver.ConnConfig
	d := newFake("embed", "embed")
	d.got = &got
	r := driver.NewRegistry()
	if err := r.Add(d); err != nil {
		t.Fatal(err)
	}
	var e driver.Embedder = keyEmbedder{key: driver.NewSecret("embedder-secret")}
	if _, err := r.Open(context.Background(), driver.ConnConfig{DSN: "embed://x", Embedder: e}); err != nil {
		t.Fatal(err)
	}
	if got.Embedder == nil || got.Embedder.Dimensions() != 2 {
		t.Fatalf("Open got Embedder %v, want the host's", got.Embedder)
	}
}

// Redaction ignores the Embedder field, and printing a ConnConfig that
// holds one never reveals the embedder's credential.
func TestConnConfigEmbedderNotPrinted(t *testing.T) {
	cfg := driver.ConnConfig{DSN: "postgres://u:hunter22@h/db", Embedder: keyEmbedder{key: driver.NewSecret("embedder-secret")}}
	if got := driver.RedactMessage("dial postgres://u:hunter22@h/db", cfg); strings.Contains(got, "hunter22") {
		t.Fatalf("RedactMessage = %q", got)
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if s := fmt.Sprintf(verb, cfg); strings.Contains(s, "embedder-secret") {
			t.Errorf("%s printed the embedder credential: %s", verb, s)
		}
	}
}

// VectorQuery names its metric; the empty metric is the driver's default.
func TestVectorMetrics(t *testing.T) {
	q := driver.VectorQuery{Vector: []float32{1}, Metric: driver.MetricInnerProduct}
	if q.Metric != "ip" || driver.MetricL2 != "l2" || driver.MetricCosine != "cosine" || driver.MetricDefault != "" {
		t.Fatalf("metric names changed: %q %q %q %q", q.Metric, driver.MetricL2, driver.MetricCosine, driver.MetricDefault)
	}
	if !strings.Contains(driver.NeedEmbedderHint, "ai.embedder") {
		t.Fatalf("hint = %q", driver.NeedEmbedderHint)
	}
}
